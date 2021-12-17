package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openshift/library-go/pkg/serviceability"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/monitor/health"
)

const (
	DefaultHTTPDialTimeout     = 2 * time.Second
	DefaultHealthCheckInterval = 5 * time.Second
	DefaultEndpoint            = "https://localhost:2379"
	DefaultLogLevel            = 2
	DefaultLogOutputs          = "stderr"
	DefaultStaticPodVersion    = 0

	// DefaultLogRotationConfig is the default configuration used for log rotation.
	// Log rotation is disabled by default.
	// MaxSize    = 100 // MB
	// MaxAge     = 0 // days (no limit)
	// MaxBackups = 10
	// LocalTime  = false // use computers local time, UTC by default
	// Compress   = true // compress the rotated log in gzip format
	DefaultLogRotationConfig = `{"maxsize": 100, "maxage": 0, "maxbackups": 10, "localtime": false, "compress": true}`
)

// defaults used by kube-apiserver
const keepaliveTime = 30 * time.Second
const keepaliveTimeout = 10 * time.Second
const dialTimeout = 20 * time.Second

type monitorOpts struct {
	errOut           io.Writer
	Targets          string
	TargetPath       string
	dialTimeout      time.Duration
	interval         time.Duration
	logLevel         int
	logOutputs       []string
	podName          string
	staticPodVersion int
	clientCertFile   string
	clientKeyFile    string
	clientCACertFile string

	// enableLogRotation enables log rotation of a single LogOutputs file target.
	enableLogRotation bool
	// logRotationConfigJSON is a passthrough allowing a log rotation JSON config to be passed directly.
	LogRotationConfigJSON string `json:"log-rotation-config-json"`
}

type Monitor struct {
	wg           sync.WaitGroup
	interval     time.Duration
	healthChecks []*health.Check
}

func NewMonitorCommand(errOut io.Writer) *cobra.Command {
	monitorOpts := &monitorOpts{
		errOut:   errOut,
		logLevel: DefaultLogLevel,
	}
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Performs periodic health checks and logs service status",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func(ctx context.Context) error) {
				if err := fn(context.Background()); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
						fmt.Fprint(monitorOpts.errOut, err.Error())
					}
				}
			}
			must(monitorOpts.Validate)
			must(monitorOpts.Run)
		},
	}
	monitorOpts.AddFlags(cmd.Flags())
	return cmd
}

func (o *monitorOpts) AddFlags(fs *pflag.FlagSet) {
	fs.DurationVar(&o.dialTimeout, "dial-timeout", DefaultHTTPDialTimeout, "Dial timeout for the client. Default 2s")
	fs.DurationVar(&o.interval, "probe-interval", DefaultHealthCheckInterval, "Frequency of health checks. Default 5s")
	fs.StringVar(&o.Targets, "targets", "", "Comma separated listed of targets to perform health checks against.")
	fs.StringVar(&o.TargetPath, "target-path", "", "Static pod resource path which contains etcd targets.")
	fs.StringSliceVar(&o.logOutputs, "log-outputs", []string{DefaultLogOutputs}, "Logger output targets. Default stderr")
	fs.BoolVar(&o.enableLogRotation, "enable-log-rotation", false, "Enable log rotation of a single log-outputs file target.")
	fs.StringVar(&o.LogRotationConfigJSON, "log-rotation-config-json", DefaultLogRotationConfig, "Configures log rotation if enabled with a JSON logger config. Default: MaxSize=100(MB), MaxAge=0(days,no limit), MaxBackups=10(no limit), LocalTime=false(UTC), Compress=false(true)")
	fs.StringVar(&o.podName, "pod-name", os.Getenv("POD_NAME"), "Name of the pod the health probe will monitor.")
	fs.IntVar(&o.staticPodVersion, "static-pod-version", DefaultStaticPodVersion, "The revision of the current static pod.")
	fs.StringVar(&o.clientCertFile, "cert-file", "", "Health probe TLS client certificate file. (required)")
	fs.StringVar(&o.clientKeyFile, "key-file", "", "Health probe TLS client key file. (required)")
	fs.StringVar(&o.clientCACertFile, "cacert-file", "", "Health probe TLS client CA certificate file. (required)")
}

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func (o *monitorOpts) Validate(_ context.Context) error {
	if len(o.clientKeyFile) == 0 {
		return errors.New("missing required flag: --key-file")
	}
	if len(o.clientCertFile) == 0 {
		return errors.New("missing required flag: --cert-file")
	}
	if len(o.clientCACertFile) == 0 {
		return errors.New("missing required flag: --cacert-file")
	}
	if len(o.TargetPath) != 0 && len(o.Targets) != 0 {
		return errors.New("--target-path and --targets flags can not be used together")
	}
	return nil
}

func (o *monitorOpts) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	ctx, cancel := context.WithCancel(ctx)

	// handle teardown
	shutdownHandler := make(chan os.Signal, 2)
	signal.Notify(shutdownHandler, shutdownSignals...)
	go func() {
		select {
		case <-shutdownHandler:
			klog.Infof("Received SIGTERM or SIGINT signal, shutting down.")
			close(shutdownHandler)
			cancel()
		case <-ctx.Done():
			klog.Infof("Context has been cancelled, shutting down.")
			close(shutdownHandler)
			cancel()
		}
	}()

	// enable profiler
	serviceability.StartProfiler()
	defer serviceability.Profile(os.Getenv("OPENSHIFT_PROFILE")).Stop()

	lg, err := health.GetZapLogger(health.LoglevelToZap(o.logLevel), o.logOutputs, o.enableLogRotation, o.LogRotationConfigJSON)
	if err != nil {
		return err
	}

	// TODO make health checks configurable by flag(s)
	monitor, err := o.newMonitor(ctx, lg,
		WithSingleTargetHealthCheck(
			health.WithSerializedReadSingleTarget(),
			health.WithGRPCReadySingleTarget(),
			health.WithQuorumReadSingleTarget(),
		),
		WithMultiTargetHealthCheck(
			health.WithQuorumRead(),
			// TODO this is needed to isolate OVN type failures.
			// health.WithNodeICMP(),
		),
	)
	if err != nil {
		errMsg := "failed to create new health monitor"
		lg.Error(errMsg, zap.Error(err))
		return fmt.Errorf("%s: %v", errMsg, err)
	}

	lg.Info("health monitor is starting",
		zap.String("pod", o.podName),
		zap.Int("static-pod-version", o.staticPodVersion),
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitor.Schedule(ctx, &wg)
	}()
	wg.Wait()
	lg.Info("health monitor is shutting down",
		zap.String("pod", o.podName),
		zap.Int("static-pod-version", o.staticPodVersion),
	)
	return nil
}

// newMonitor creates a series of health checks. Health check probes can be single or multi target.
func (o *monitorOpts) newMonitor(ctx context.Context, lg *zap.Logger, singleTargetChecks []health.CheckFunc, multiTargetChecks []health.CheckFunc) (*Monitor, error) {
	var healthChecks []*health.Check

	tlsInfo := transport.TLSInfo{
		CertFile:      o.clientCertFile,
		KeyFile:       o.clientKeyFile,
		TrustedCAFile: o.clientCACertFile,
	}

	targets, err := o.getTargets()
	if err != nil {
		lg.Error("failed to get targets", zap.Error(err))
		return nil, err
	}

	lg.Info("monitor targets registered", zap.Strings("endpoints", targets))

	// Create single target checks one check per target
	for _, target := range targets {
		// one client per target to eliminate lock racing
		client, err := newETCD3Client(ctx, tlsInfo, targets)
		if err != nil {
			lg.Error("failed to create etcd client", zap.Error(err))
			return nil, err
		}
		// pin endpoint for check
		client.SetEndpoints(target)
		for _, healthCheckFunc := range singleTargetChecks {
			healthCheck := health.NewCheck(lg, client, []string{target})
			healthCheck.Probe = healthCheckFunc
			healthChecks = append(healthChecks, healthCheck)
		}
	}

	if len(targets) > 1 {
		client, err := newETCD3Client(ctx, tlsInfo, targets)
		if err != nil {
			lg.Error("failed to create etcd client", zap.Error(err))
			return nil, err
		}

		for _, healthCheckFunc := range multiTargetChecks {
			healthCheck := health.NewCheck(lg, client, targets)
			healthCheck.Probe = healthCheckFunc
			healthChecks = append(healthChecks, healthCheck)
		}
	}

	monitor := &Monitor{
		interval:     o.interval,
		healthChecks: healthChecks,
	}

	return monitor, nil
}

func (m *Monitor) Schedule(ctx context.Context, wg *sync.WaitGroup) {
	for _, healthCheck := range m.healthChecks {
		wg.Add(1)
		go func(healthCheck *health.Check) {
			defer wg.Done()
			defer healthCheck.LogTermination()
			wait.UntilWithContext(ctx,
				func(context.Context) {
					healthCheck.Probe(ctx, healthCheck)
				},
				m.interval)
		}(healthCheck)
	}
}

func (o *monitorOpts) getTargets() ([]string, error) {
	if len(o.Targets) > 0 {
		return strings.Split(o.Targets, ","), nil
	}
	data, err := configMapLoad(o.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load configmap from disk: %v", err)
	}
	var targets []string
	for _, target := range data {
		targets = append(targets, fmt.Sprintf("https://%s:2379", target))
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no targets found in target path: %s", o.TargetPath)
	}

	return targets, nil
}

func WithMultiTargetHealthCheck(checks ...health.CheckFunc) []health.CheckFunc {
	return checks
}

func WithSingleTargetHealthCheck(checks ...health.CheckFunc) []health.CheckFunc {
	return checks
}

func newETCD3Client(ctx context.Context, tlsInfo transport.TLSInfo, endpoints []string) (*clientv3.Client, error) {

	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}
	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
	}

	cfg := &clientv3.Config{
		DialTimeout:          dialTimeout,
		DialOptions:          dialOptions,
		DialKeepAliveTime:    keepaliveTime,
		DialKeepAliveTimeout: keepaliveTimeout,
		Endpoints:            endpoints,
		TLS:                  tlsConfig,
		Context:              ctx,
	}

	return clientv3.New(*cfg)
}

// configMapLoad reads the "Data" of a ConfigMap from a particular VolumeMount.
// Borrowed from knative 0:)
func configMapLoad(p string) (map[string]string, error) {
	data := make(map[string]string)
	err := filepath.Walk(p, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		for info.Mode()&os.ModeSymlink != 0 {
			dirname := filepath.Dir(p)
			p, err = os.Readlink(p)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(p) {
				p = path.Join(dirname, p)
			}
			info, err = os.Lstat(p)
			if err != nil {
				return err
			}
		}
		if info.IsDir() {
			return nil
		}
		b, err := ioutil.ReadFile(p)
		if err != nil {
			return err
		}
		data[info.Name()] = string(b)
		return nil
	})
	return data, err
}

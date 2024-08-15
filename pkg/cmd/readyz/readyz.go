package readyz

import (
	"context"
	"crypto/tls"
	"errors"
	goflag "flag"
	"fmt"
	"go.etcd.io/etcd/client/pkg/v3/tlsutil"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"

	"github.com/spf13/cobra"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"
)

const (
	defaultListenPort      = 9980
	defaultHTTPDialTimeout = 2 * time.Second
	defaultEndpoint        = "https://localhost:2379"
	// keepalive defaults used by kube-apiserver
	// https://github.com/kubernetes/apiserver/blob/de6ba2aa0a752d077719fcee186291d7af73a825/pkg/storage/storagebackend/factory/etcd3.go#L55-L56
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 10 * time.Second
)

type readyzOpts struct {
	listenPort       uint16
	dialTimeout      time.Duration
	targetEndpoint   string
	servingCertFile  string
	servingKeyFile   string
	clientCertFile   string
	clientKeyFile    string
	clientCACertFile string
	cipherSuites     []string

	clientPool *etcdcli.EtcdClientPool
}

func newReadyzOpts() *readyzOpts {
	return &readyzOpts{
		listenPort:     defaultListenPort,
		dialTimeout:    defaultHTTPDialTimeout,
		targetEndpoint: defaultEndpoint,
		cipherSuites: []string{
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			"TLS_AES_128_GCM_SHA256",
			"TLS_AES_256_GCM_SHA384",
			"TLS_CHACHA20_POLY1305_SHA256",
		},
	}
}

// NewReadyzCommand creates a readyz command that runs as an http-get readiness server alongside the etcd member container
func NewReadyzCommand() *cobra.Command {
	opts := newReadyzOpts()
	cmd := &cobra.Command{
		Use:   "readyz",
		Short: "Serve the HTTP /readyz endpoint health check for an etcd member",
		Run: func(cmd *cobra.Command, args []string) {
			defer klog.Flush()

			if err := opts.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := opts.Run(); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd)
	return cmd
}

func (r *readyzOpts) AddFlags(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.Uint16Var(&r.listenPort, "listen-port", r.listenPort, "Listen on this port. Default 9980")
	fs.StringSliceVar(&r.cipherSuites, "listen-cipher-suites", r.cipherSuites, "readyZ server TLS cipher suites.")
	fs.DurationVar(&r.dialTimeout, "dial-timeout", r.dialTimeout, "Dial timeout for the client. Default 2s")
	fs.StringVar(&r.targetEndpoint, "target", r.targetEndpoint, "Target endpoint to perform health check against. Default https://localhost:2379")
	fs.StringVar(&r.servingCertFile, "serving-cert-file", r.servingCertFile, "Health probe server TLS client certificate file. (required)")
	fs.StringVar(&r.servingKeyFile, "serving-key-file", r.servingKeyFile, "Health probe server TLS client key file. (required)")
	fs.StringVar(&r.clientCertFile, "client-cert-file", r.clientCertFile, "Etcd TLS client certificate file. (required)")
	fs.StringVar(&r.clientKeyFile, "client-key-file", r.clientKeyFile, "Etcd TLS client key file. (required)")
	fs.StringVar(&r.clientCACertFile, "client-cacert-file", r.clientCACertFile, "Etcd TLS client CA certificate file. (required)")
	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

// Validate verifies the inputs.
func (r *readyzOpts) Validate() error {
	if len(r.targetEndpoint) == 0 {
		return errors.New("missing required flag: --target")
	}
	if len(r.servingCertFile) == 0 {
		return errors.New("missing required flag: --serving-cert-file")
	}
	if len(r.servingKeyFile) == 0 {
		return errors.New("missing required flag: --serving-key-file")
	}
	if len(r.clientCertFile) == 0 {
		return errors.New("missing required flag: --client-cert-file")
	}
	if len(r.clientKeyFile) == 0 {
		return errors.New("missing required flag: --client-key-file")
	}
	if len(r.clientCACertFile) == 0 {
		return errors.New("missing required flag: --client-cacert-file")
	}

	_, err := tlsutil.GetCipherSuites(r.cipherSuites)
	if err != nil {
		return fmt.Errorf("invalid TLS cipher suites passed via --listen-cipher-suites: %v, err=%w", r.cipherSuites, err)
	}

	// ensure the cert files really exist, sometimes during installation we might not have the certificates created yet
	// we should exit this container with an error instead of the readiness probe during that time
	for _, certFile := range []string{r.clientKeyFile, r.clientCertFile, r.clientCACertFile, r.servingKeyFile, r.servingCertFile} {
		_, err = os.Lstat(certFile)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("unable to find cert [%s]: %w", certFile, err)
			}
			return err
		}
	}

	return nil
}

// Run contains the logic of the readyz command which checks the health of the etcd member
func (r *readyzOpts) Run() error {
	clientPool := etcdcli.NewEtcdClientPool(
		// client factory
		func() (*clientv3.Client, error) {
			return r.newETCD3Client(context.Background(), r.targetEndpoint)
		},
		// endpoints
		func() ([]string, error) {
			return []string{r.targetEndpoint}, nil
		},
		// health check
		func(client *clientv3.Client) error {
			_, err := client.Status(context.Background(), r.targetEndpoint)
			return err
		},
		// closing
		func(client *clientv3.Client) error {
			if client == nil {
				return nil
			}
			klog.Infof("closing cached client")
			return client.Close()
		})

	r.clientPool = clientPool

	shutdownCtx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", r.getReadyzHandlerFunc(shutdownCtx))
	// Handle the /healthz endpoint as well since the static pod controller's guard pods check the /healthz endpoint
	// https://github.com/openshift/library-go/blob/edab248e63516c65a93467eaa8224c86d69f5de9/pkg/operator/staticpod/controller/guard/manifests/guard-pod.yaml#L44
	mux.HandleFunc("/healthz", r.getReadyzHandlerFunc(shutdownCtx))

	addr := fmt.Sprintf("0.0.0.0:%d", r.listenPort)
	klog.Infof("Listening on %s", addr)

	suites, err := tlsutil.GetCipherSuites(r.cipherSuites)
	if err != nil {
		cancel()
		return err
	}

	httpServer := &http.Server{
		Addr:        addr,
		Handler:     mux,
		BaseContext: func(_ net.Listener) context.Context { return shutdownCtx },
		TLSConfig:   &tls.Config{CipherSuites: suites},
	}
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Infof("Received SIGTERM or SIGINT signal, shutting down readyz server.")
		err := httpServer.Shutdown(shutdownCtx)
		if err != nil {
			klog.Errorf("Error while shutting down readyz server: %v", err)
		}
	}()

	c := net.ListenConfig{}
	c.Control = permitAddressReuse
	ln, err := c.Listen(shutdownCtx, "tcp", addr)
	if err != nil {
		return err
	}
	err = httpServer.ServeTLS(ln, r.servingCertFile, r.servingKeyFile)
	if err == http.ErrServerClosed {
		err = nil
		<-shutdownCtx.Done()
	}
	return err
}

func (r *readyzOpts) getReadyzHandlerFunc(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		etcdClient, err := r.clientPool.Get()
		if err != nil {
			klog.V(2).Infof("failed to establish etcd client: %v", err)
			http.Error(w, fmt.Sprintf("failed to establish etcd client: %v", err), http.StatusServiceUnavailable)
			return
		}

		defer r.clientPool.Return(etcdClient)

		timeout, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		// we solely do serializable requests to the local instance, as we don't want the readiness of individual
		// etcd members to rely on an existing quorum to do linearized requests.
		_, err = etcdClient.Get(timeout, "health", clientv3.WithSerializable())
		if err != nil {
			klog.V(2).Infof("failed to get member health key: %v", err)
			http.Error(w, fmt.Sprintf("failed to get member health key: %v", err), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func permitAddressReuse(network, addr string, conn syscall.RawConn) error {
	return conn.Control(func(fd uintptr) {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			klog.Warningf("failed to set SO_REUSEADDR on socket: %v", err)
		}
	})
}

func (r *readyzOpts) newETCD3Client(ctx context.Context, endpoint string) (*clientv3.Client, error) {
	tlsInfo := transport.TLSInfo{
		CertFile:      r.clientCertFile,
		KeyFile:       r.clientKeyFile,
		TrustedCAFile: r.clientCACertFile,
	}

	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}
	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
	}

	cfg := &clientv3.Config{
		DialTimeout:          r.dialTimeout,
		DialOptions:          dialOptions,
		DialKeepAliveTime:    keepaliveTime,
		DialKeepAliveTimeout: keepaliveTimeout,
		Endpoints:            []string{endpoint},
		TLS:                  tlsConfig,
		Context:              ctx,
	}

	return clientv3.New(*cfg)
}

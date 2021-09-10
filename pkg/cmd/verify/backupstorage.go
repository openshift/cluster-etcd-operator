package verify

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/transport"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	defaultEndpoint       = "https://localhost:2379"
	defaultBackupPath     = "/etc/kubernetes/cluster-backup"
	defaultCertFilePath   = "/var/run/secrets/etcd-client/tls.crt"
	defaultKeyFilePath    = "/var/run/secrets/etcd-client/tls.key"
	defaultCaCertFilePath = "/var/run/configmaps/etcd-ca/ca-bundle.crt"

	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 10 * time.Second
	dialTimeout      = 20 * time.Second
)

type verifyBackupStorage struct {
	errOut io.Writer

	endpoints        string
	backupPath       string
	clientCertFile   string
	clientKeyFile    string
	clientCACertFile string
}

// NewVerifyBackupStorage perform checks against the local filesystem and compares the available storage bytes with the
// estimated size as reported by EndpointStatus.
func NewVerifyBackupStorage(errOut io.Writer) *cobra.Command {
	verifyBackupStorage := &verifyBackupStorage{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "backup-storage",
		Short: "performs checks to ensure storage is adequate for backup state",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func(ctx context.Context) error) {
				if err := fn(context.Background()); err != nil {
					fmt.Fprint(verifyBackupStorage.errOut, err.Error())
					os.Exit(1)
				}
			}
			must(verifyBackupStorage.Run)
		},
	}

	verifyBackupStorage.AddFlags(cmd.Flags())
	return cmd
}

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func (v *verifyBackupStorage) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&v.endpoints, "endpoints", defaultEndpoint, "Comma separated listed of targets to perform health checks against. Default https://localhost:2379")
	fs.StringVar(&v.backupPath, "backup-path", defaultBackupPath, "path to verify storage requirements.")
	fs.StringVar(&v.clientCertFile, "cert-file", defaultCertFilePath, "etcd client certificate file.")
	fs.StringVar(&v.clientKeyFile, "key-file", defaultKeyFilePath, "etcd client key file.")
	fs.StringVar(&v.clientCACertFile, "cacert-file", defaultCaCertFilePath, "etcd client CA certificate file.")
}

func (v *verifyBackupStorage) Run(ctx context.Context) error {
	defer utilruntime.HandleCrash()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

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

	waitDuration := 1 * time.Second
	timeoutDuration := 10 * time.Second

	// Perform reasonable retry on non-fatal errors
	err := wait.Poll(
		waitDuration,
		timeoutDuration,
		func() (bool, error) {
			return v.isStorageAdequate(ctx)
		})
	if err != nil {
		return err
	}

	return nil
}

func (v *verifyBackupStorage) isStorageAdequate(ctx context.Context) (bool, error) {
	tlsInfo := transport.TLSInfo{
		CertFile:      v.clientCertFile,
		KeyFile:       v.clientKeyFile,
		TrustedCAFile: v.clientCACertFile,
	}
	cli, err := newETCD3Client(ctx, tlsInfo)
	if err != nil {
		klog.Warningf("failed to create client: %v", err)
		return false, nil
	}
	defer cli.Close()

	members, err := cli.MemberList(ctx)
	if err != nil {
		klog.Warningf("failed checking member list: %v", err)
		return false, nil
	}

	dbSizeBytes, err := getBackupSizeBytes(ctx, cli, members.Members)
	if err != nil {
		klog.Warningf("failed checking backup size: %v", err)
		return false, nil
	}

	fsAvailableBytes, err := getPathAvailableSpaceBytes(v.backupPath)
	if err != nil {
		return false, err
	}

	requiredBytes := 2 * dbSizeBytes
	if requiredBytes > fsAvailableBytes {
		return false, fmt.Errorf("available storage is not adequate for path: %q, required bytes: %d, available bytes %d\n", v.backupPath, requiredBytes, fsAvailableBytes)
	}

	klog.Infof("Path %s, required storage bytes: %d, available %d\n", v.backupPath, requiredBytes, fsAvailableBytes)
	return true, nil
}

func newETCD3Client(ctx context.Context, tlsInfo transport.TLSInfo) (*clientv3.Client, error) {
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
		Endpoints:            []string{defaultEndpoint},
		TLS:                  tlsConfig,
		Context:              ctx,
	}

	return clientv3.New(*cfg)
}

// getBackupSizeBytes asks the etcd leader for DbSize as an approximation for how large the backup state will be. If
// leader is not found in the list we use the largest DBSize.
func getBackupSizeBytes(ctx context.Context, cli *clientv3.Client, members []*etcdserverpb.Member) (int64, error) {
	var errs []error
	var endpointStatus []*clientv3.StatusResponse
	for _, member := range members {
		if !etcdcli.HasStarted(member) {
			continue
		}
		status, err := cli.Status(ctx, member.ClientURLs[0])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// best effort use leader
		if status.Leader != status.Header.MemberId {
			return status.DbSize, nil
		}
		endpointStatus = append(endpointStatus, status)
	}

	// If no leader responds use the largest of endpoints queried.
	if len(endpointStatus) > 0 {
		var largestDBSize int64
		for _, status := range endpointStatus {
			if status.DbSize > largestDBSize {
				largestDBSize = status.DbSize
			}
		}
		return largestDBSize, nil
	}

	if len(errs) > 0 {
		return 0, utilerrors.NewAggregate(errs)
	}

	return 0, fmt.Errorf("endpoint status: DBSize check failed")
}

func getPathAvailableSpaceBytes(path string) (int64, error) {
	// Verify path exists
	if _, err := os.Stat(path); err != nil {
		return 0, fmt.Errorf("verification of backup path failed: %w", err)
	}
	var stat unix.Statfs_t
	err := unix.Statfs(path, &stat)
	if err != nil {
		return 0, fmt.Errorf("filesystem status: %w", err)
	}

	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available == 0 {
		return 0, fmt.Errorf("filesystem status: no available bytes")
	}

	return available, nil
}

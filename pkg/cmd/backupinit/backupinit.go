package backupinit

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/clientv3/snapshot"
	"go.etcd.io/etcd/pkg/transport"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"k8s.io/klog"
)

type backupOptions struct {
	endpoint  string
	configDir string
	dataDir   string
	backupDir string
	errOut    io.Writer
}

func NewBackupInitCommand(errOut io.Writer) *cobra.Command {
	backupOpts := &backupOptions{
		errOut: errOut,
	}
	backupCmd := &cobra.Command{
		Use:   "backup-init",
		Short: "Backs up a snapshot of etcd database",
		Run: func(backupCmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if backupCmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(backupOpts.errOut, err.Error())
				}
			}

			must(backupOpts.Run)
		},
	}
	backupOpts.AddFlags(backupCmd.Flags())
	return backupCmd
}

func (r *backupOptions) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
	fs.StringVar(&r.endpoint, "endpoint", "https://127.0.0.1:2379", "etcd endpoint to take snapshot")
	fs.StringVar(&r.backupDir, "backup-dir", "/backup", "Path to the backup source directory")
}

func (r *backupOptions) Run() error {
	if err := backup(r); err != nil {
		klog.Errorf("run: backup failed: %v", err)
		return err
	}
	klog.Infof("backup-dir is: %s", r.backupDir)
	return nil
}

func backup(r *backupOptions) error {
	//TODO before we snapshot verify that revisions defined in this pod are latest
	cfg, err := getClientCfg(r.endpoint)
	if err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	// Trying to match the output file formats with the formats of the current cluster-backup.sh script
	dateString := time.Now().Format("2006-01-02_150405")
	snapshotOutFile := "snapshot_" + dateString + ".db"

	// Save snapshot
	if err := saveSnapshot(cfg, filepath.Join(r.backupDir, snapshotOutFile)); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}

	return nil
}

func saveSnapshot(cfg *clientv3.Config, path string) error {
	lg, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}
	sp := snapshot.NewV3(lg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sp.Save(ctx, *cfg, path); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}

	klog.Info("saved snapshot to path: %s", path)
	return nil
}

func getClientCfg(endpoint string) (*clientv3.Config, error) {
	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
	}

	tlsInfo := transport.TLSInfo{
		CertFile:      "/var/run/secrets/etcd-client/tls.crt",
		KeyFile:       "/var/run/secrets/etcd-client/tls.key",
		TrustedCAFile: "/var/run/configmaps/etcd-ca/ca-bundle.crt",
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("getClientCfg failed: %w", err)
	}

	cfg := &clientv3.Config{
		DialOptions: dialOptions,
		Endpoints:   []string{endpoint},
		DialTimeout: 10 * time.Second,
		TLS:         tlsConfig,
	}
	return cfg, nil
}

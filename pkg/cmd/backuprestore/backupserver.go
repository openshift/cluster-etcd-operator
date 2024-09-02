package backuprestore

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type backupServer struct {
	schedule string
	timeZone string
	enabled  bool
	backupOptions
	prune.PruneOpts
}

func NewBackupServer(ctx context.Context) *cobra.Command {
	backupSrv := &backupServer{
		backupOptions: backupOptions{errOut: os.Stderr},
	}

	cmd := &cobra.Command{
		Use:   "backup-server",
		Short: "Backs up a snapshot of etcd database and static pod resources without config",
		Run: func(cmd *cobra.Command, args []string) {

			if err := backupSrv.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := backupSrv.Run(ctx); err != nil {
				klog.Fatal(err)
			}
		},
	}
	backupSrv.AddFlags(cmd)
	return cmd
}

func (b *backupServer) AddFlags(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.BoolVar(&b.enabled, "enabled", false, "enable backup server")
	fs.StringVar(&b.schedule, "schedule", "", "schedule specifies the cron schedule to run the backup")
	fs.StringVar(&b.timeZone, "timezone", "", "timezone specifies the timezone of the cron schedule to run the backup")

	cobra.MarkFlagRequired(fs, "enabled")

	b.backupOptions.AddFlags(fs)
	b.PruneOpts.AddFlags(cmd)
}

func (b *backupServer) Validate() error {
	return nil
}

func (b *backupServer) Run(ctx context.Context) error {
	// handle teardown
	cCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	signal.NotifyContext(cCtx, shutdownSignals...)

	<-ctx.Done()

	return nil
}

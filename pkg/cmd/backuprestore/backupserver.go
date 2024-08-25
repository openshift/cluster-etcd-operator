package backuprestore

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/adhocore/gronx/pkg/tasker"
	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

const (
	backupVolume = "/var/backup/etcd/"
	backupFolder = "current-backup"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type backupServer struct {
	schedule  string
	timeZone  string
	enabled   bool
	scheduler *tasker.Tasker
	backupOptions
	prune.PruneOpts
}

func NewBackupServer(ctx context.Context) *cobra.Command {
	backupSrv := &backupServer{
		backupOptions: backupOptions{errOut: os.Stderr},
		PruneOpts:     prune.PruneOpts{RetentionType: "None"},
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
	if !b.enabled {
		klog.Infof("backup-server is disabled")
		return nil
	}

	if len(b.schedule) == 0 {
		err := errors.New("missing required flag: --schedule")
		klog.Error(err)
		return err
	}

	b.backupOptions.backupDir = backupVolume + backupFolder
	err := b.backupOptions.Validate()
	if err != nil {
		klog.Error(err)
		return err
	}

	err = b.PruneOpts.Validate()
	if err != nil {
		klog.Error(err)
		return err
	}

	return nil
}

func (b *backupServer) Run(ctx context.Context) error {
	b.scheduler = tasker.New(tasker.Option{
		Verbose: true,
		Tz:      b.timeZone,
	})
	// handle teardown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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

	doneCh := make(chan struct{})

	if b.enabled {
		err := b.scheduleBackup()
		if err != nil {
			return err
		}
		go func() {
			b.scheduler.Run()
			doneCh <- struct{}{}
		}()
	}

	for {
		select {
		case <-doneCh:
			klog.Infof("scheduler has been terminated - doneCh")
		case <-ctx.Done():
			klog.Infof("context has been terminated - ctx.DONE()")
		}
	}
}

func (b *backupServer) scheduleBackup() error {
	var err error
	b.scheduler.Task(b.schedule, func(ctx context.Context) (int, error) {
		err = backup(&b.backupOptions)
		if err != nil {
			return 1, err
		}

		err = b.PruneOpts.Run()
		if err != nil {
			return 1, err
		}

		return 0, err
	}, false)
	return err
}

package backuprestore

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const backupVolume = "/var/lib/etcd-auto-backup"

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type backupRunner interface {
	runBackup(opts *backupOptions) error
}

type backupRunnerImpl struct{}

func (b backupRunnerImpl) runBackup(opts *backupOptions) error {
	dateString := time.Now().Format("2006-01-02_150405")
	opts.backupDir = backupVolume + dateString
	err := backup(opts)
	if err != nil {
		return err
	}

	return nil
}

type backupServer struct {
	schedule     string
	timeZone     string
	enabled      bool
	cronSchedule cron.Schedule
	backupOptions
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

	backupSrv.AddFlags(cmd.Flags())
	return cmd
}

func (b *backupServer) AddFlags(fs *pflag.FlagSet) {
	fs.BoolVar(&b.enabled, "enabled", false, "enable backup server")
	fs.StringVar(&b.schedule, "schedule", "", "schedule specifies the cron schedule to run the backup")
	fs.StringVar(&b.timeZone, "timezone", "", "timezone specifies the timezone of the cron schedule to run the backup")

	b.backupOptions.AddFlags(fs)
}

func (b *backupServer) Validate() error {
	if !b.enabled {
		klog.Infof("backup-server is disabled")
		return nil
	}

	cronSchedule, err := cron.ParseStandard(b.schedule)
	if err != nil {
		return fmt.Errorf("error parsing backup schedule %v: %w", b.schedule, err)
	}
	b.cronSchedule = cronSchedule

	b.backupOptions.backupDir = backupVolume
	err = b.backupOptions.Validate()
	if err != nil {
		return fmt.Errorf("error validating backup %v: %w", b.backupOptions, err)
	}

	return nil
}

func (b *backupServer) Run(ctx context.Context) error {
	// handle teardown
	cCtx, cancel := signal.NotifyContext(ctx, shutdownSignals...)
	defer cancel()

	if b.enabled {
		bck := backupRunnerImpl{}
		err := b.scheduleBackup(cCtx, bck)
		if err != nil {
			return err
		}
	}

	<-ctx.Done()
	return nil
}

func (b *backupServer) scheduleBackup(ctx context.Context, bck backupRunner) error {
	ticker := time.NewTicker(time.Until(b.cronSchedule.Next(time.Now())))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := bck.runBackup(&b.backupOptions)
			if err != nil {
				klog.Errorf("error running backup: %v", err)
				return err
			}
			ticker.Reset(time.Until(b.cronSchedule.Next(time.Now())))
		case <-ctx.Done():
			return nil
		}
	}
}

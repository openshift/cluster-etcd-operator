package backuprestore

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	cron "github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const backupVolume = "/var/backup/etcd/"

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

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
		pErr := fmt.Errorf("error parsing backup schedule %v: %w", b.schedule, err)
		klog.Error(pErr)
		return pErr
	}
	b.cronSchedule = cronSchedule

	b.backupOptions.backupDir = backupVolume
	err = b.backupOptions.Validate()
	if err != nil {
		klog.Error(err)
		return err
	}

	return nil
}

func (b *backupServer) Run(ctx context.Context) error {
	// handle teardown
	cCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	signal.NotifyContext(cCtx, shutdownSignals...)

	if b.enabled {
		err := b.scheduleBackup(cCtx, b.cronSchedule)
		if err != nil {
			return err
		}
	}

	<-ctx.Done()
	return nil
}

func (b *backupServer) scheduleBackup(ctx context.Context, schedule cron.Schedule) error {
	ticker := time.NewTicker(time.Until(schedule.Next(time.Now())))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := b.runBackup()
			if err != nil {
				klog.Errorf("error running backup: %v", err)
				return err
			}
			ticker.Reset(time.Until(schedule.Next(time.Now())))
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *backupServer) runBackup() error {
	dateString := time.Now().Format("2006-01-02_150405")
	b.backupOptions.backupDir = backupVolume + dateString
	err := backup(&b.backupOptions)
	if err != nil {
		return err
	}

	return nil
}

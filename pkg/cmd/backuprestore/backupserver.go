package backuprestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"
	"github.com/robfig/cron"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"
)

const (
	backupVolume = "/var/backup/etcd/"
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
		PruneOpts: prune.PruneOpts{
			RetentionType: "None",
			BackupPath:    backupVolume,
		},
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

	b.backupOptions.backupDir = backupVolume
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
	// handle teardown
	cCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	signal.NotifyContext(cCtx, shutdownSignals...)

	if b.enabled {
		schedule, err := cron.ParseStandard(b.schedule)
		if err != nil {
			pErr := fmt.Errorf("error parsing backup schedule %v: %w", b.schedule, err)
			klog.Error(pErr)
			return pErr
		}

		err = b.scheduleBackup(cCtx, schedule)
		if err != nil {
			return err
		}
	}

	<-ctx.Done()
	return nil
}

func (b *backupServer) scheduleBackup(ctx context.Context, schedule cron.Schedule) error {
	nextRun := schedule.Next(time.Now())
	ticker := time.NewTicker(time.Until(nextRun))
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := b.runBackup()
			if err != nil {
				return err
			}
		case <-ctx.Done():
			klog.Infof("context deadline has exceeded")
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

	err = b.PruneOpts.Run()
	if err != nil {
		return err
	}

	return nil
}

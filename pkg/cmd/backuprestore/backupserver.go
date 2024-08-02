package backuprestore

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/adhocore/gronx/pkg/tasker"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
}

func NewBackupServer(ctx context.Context) *cobra.Command {
	backupSrv := &backupServer{
		backupOptions: backupOptions{errOut: os.Stderr},
	}

	cmd := &cobra.Command{
		Use:   "backup-server",
		Short: "Backs up a snapshot of etcd database and static pod resources without config",
		Run: func(cmd *cobra.Command, args []string) {

			klog.Infof("hello from backup server :) ")

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
	cobra.MarkFlagRequired(fs, "enabled")

	b.backupOptions.AddFlags(fs)
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
	return b.backupOptions.Validate()
}

func (b *backupServer) Run(ctx context.Context) error {
	klog.Infof("hello from backup server Run() :) ")

	b.scheduler = tasker.New(tasker.Option{
		Verbose: true,
		Tz:      b.timeZone,
	})
	klog.Infof("hello from backup server - scheduler has been init ;)  ")
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
		klog.Infof("hello from backup server -before schedule backup()  ")
		err := b.scheduleBackup()
		if err != nil {
			return err
		}
		klog.Infof("hello from backup server -after schedule backup()  ")
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
	klog.Infof("hello from backup server -inside-begin schedule backup()  ")
	var err error
	b.scheduler.Task(b.schedule, func(ctx context.Context) (int, error) {
		err = backup(&b.backupOptions)
		return 0, err
	}, false)
	klog.Infof("hello from backup server -inside-end schedule backup()  ")
	klog.Errorf("hello from backup server -inside-end schedule backup()  error [%v]", err)
	return err
}

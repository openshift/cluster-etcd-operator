package backuprestore

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

type backupServer struct {
	schedule string
	timeZone string
	//scheduler *tasker.Tasker
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
	//fs.StringVar(&b.schedule, "schedule", "", "schedule specifies the cron schedule to run the backup")
	//fs.StringVar(&b.timeZone, "timezone", "", "timezone specifies the timezone of the cron schedule to run the backup")
	//
	//b.backupOptions.AddFlags(fs)
}

func (b *backupServer) Validate() error {
	//return b.backupOptions.Validate()
	return nil
}

func (b *backupServer) Run(ctx context.Context) error {
	//b.scheduler = tasker.New(tasker.Option{
	//	Verbose: true,
	//	Tz:      b.timeZone,
	//})

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

	//err := b.scheduleBackup()
	//if err != nil {
	//	return err
	//}
	//
	//doneCh := make(chan struct{})
	//go func() {
	//	b.scheduler.Run()
	//	doneCh <- struct{}{}
	//}()
	//
	//<-doneCh
	//return nil

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				cancel()
			case <-time.After(time.Duration(1<<63 - 1)):
				klog.Info("time elapsed ...")
			}
		}
	}()
	wg.Wait()

	return nil
}

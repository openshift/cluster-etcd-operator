package backuprestore

import (
	"context"
	"fmt"
	"io"

	"k8s.io/klog/v2"

	"github.com/adhocore/gronx/pkg/tasker"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type backupNoConfig struct {
	schedule  string
	timeZone  string
	scheduler *tasker.Tasker
	backupOptions
}

func NewBackupNoConfigCommand(errOut io.Writer) *cobra.Command {
	backupNoConf := &backupNoConfig{
		backupOptions: backupOptions{errOut: errOut},
	}

	cmd := &cobra.Command{
		Use:   "backup-server",
		Short: "Backs up a snapshot of etcd database and static pod resources without config",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(backupNoConf.errOut, err.Error())
				}
			}

			must(backupNoConf.Validate)
			must(backupNoConf.Run)
		},
	}
	backupNoConf.AddFlags(cmd.Flags())
	return cmd
}

func (b *backupNoConfig) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&b.schedule, "schedule", "", "schedule specifies the cron schedule to run the backup")
	fs.StringVar(&b.timeZone, "timezone", "", "timezone specifies the timezone of the cron schedule to run the backup")

	b.backupOptions.AddFlags(fs)
}

func (b *backupNoConfig) Validate() error {
	return b.backupOptions.Validate()
}

func (b *backupNoConfig) Run() error {
	b.scheduler = tasker.New(tasker.Option{
		Verbose: true,
		Tz:      b.timeZone,
	})

	err := b.scheduleBackup()
	if err != nil {
		return err
	}

	doneCh := make(chan struct{})
	go func() {
		b.scheduler.Run()
		doneCh <- struct{}{}
	}()

	<-doneCh
	return nil
}

func (b *backupNoConfig) scheduleBackup() error {
	var err error
	b.scheduler.Task(b.schedule, func(ctx context.Context) (int, error) {
		err = backup(&b.backupOptions)
		return 0, err
	}, false)

	return err
}

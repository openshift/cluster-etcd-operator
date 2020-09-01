package backuprestore

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"io"
	"k8s.io/klog/v2"
	"os"
	"os/signal"
)

type backupOptions struct {
	endpoints []string
	configDir string
	dataDir   string
	backupDir string
	errOut    io.Writer
}

func NewBackupCommand(errOut io.Writer) *cobra.Command {
	backupOpts := &backupOptions{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "cluster-backup",
		Short: "Backs up a snapshot of etcd database and static pod resources to a given directory",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(backupOpts.errOut, err.Error())
				}
			}

			must(backupOpts.Validate)
			must(backupOpts.Run)
		},
	}
	backupOpts.AddFlags(cmd.Flags())
	return cmd
}

func (r *backupOptions) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
	fs.StringSliceVar(&r.endpoints, "endpoints", []string{"127.0.0.1:2379"}, "etcd endpoints")
	fs.StringVar(&r.configDir, "config-dir", "/etc/kubernetes", "Path to the kubernetes config directory")
	fs.StringVar(&r.dataDir, "data-dir", "/var/lib/etcd", "Path to the data directory")
	fs.StringVar(&r.backupDir, "backup-dir", "", "Path to the directory where the backup is generated")
}

func (r *backupOptions) Validate() error {
	if len(r.backupDir) == 0 {
		return errors.New("missing required flag: --backup-dir")
	}
	return nil
}

func (r *backupOptions) Run() error {
	if err := backup(r); err != nil {
		klog.Errorf("run: backup failed: %v", err)
	}
	klog.Infof("config-dir is: %s", r.configDir)
	return nil
}

type restoreOptions struct {
	configDir string
	dataDir   string
	backupDir string
	errOut    io.Writer
}

func NewRestoreCommand(errOut io.Writer) *cobra.Command {
	restoreOpts := &restoreOptions{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "cluster-restore",
		Short: "Restores a cluster backup from a given directory",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(restoreOpts.errOut, err.Error())
				}
			}

			must(restoreOpts.Validate)
			must(restoreOpts.Run)
		},
	}
	restoreOpts.AddFlags(cmd.Flags())
	return cmd
}

func (r *restoreOptions) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
	fs.StringVar(&r.configDir, "config-dir", "/etc/kubernetes", "Path to the kubernetes config directory")
	fs.StringVar(&r.dataDir, "data-dir", "/var/lib/etcd", "Path to the data directory")
	fs.StringVar(&r.backupDir, "backup-dir", "", "Path to the directory where the backup is generated")
}

func (r *restoreOptions) Validate() error {
	if len(r.backupDir) == 0 {
		return errors.New("missing required flag: --backup-dir")
	}
	return nil
}

func (r *restoreOptions) Run() error {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		oscall := <-c
		klog.Warningf("system call:%+v", oscall)
		cancel()
	}()

	if err := restore(ctx, r); err != nil {
		klog.Errorf("run: restore failed: %v", err)
	}

	return nil
}

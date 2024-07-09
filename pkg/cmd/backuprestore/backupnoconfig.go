package backuprestore

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/klog/v2"
)

type backupNoConfig struct {
	snapshotExist bool
	retention     string
	backupOptions
}

func NewBackupNoConfigCommand(errOut io.Writer) *cobra.Command {
	backupNoConf := &backupNoConfig{
		snapshotExist: false,
		backupOptions: backupOptions{errOut: errOut},
	}
	cmd := &cobra.Command{
		Use:   "cluster-backup-no-config",
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
	b.backupOptions.AddFlags(fs)
}

func (b *backupNoConfig) Validate() error {
	return b.Validate()
}

func (b *backupNoConfig) Run() error {
	if !b.snapshotExist {
		if err := backup(&b.backupOptions); err != nil {
			klog.Errorf("run: backup failed: [%v]", err)
		}
		b.snapshotExist = true
		klog.Infof("config-dir is: %s", b.configDir)
		return nil
	}

	if err := b.copySnapshot(); err != nil {
		klog.Errorf("run: backup failed: [%v]", err)
	}

	return nil
}

func (b *backupNoConfig) copySnapshot() error {
	if !b.snapshotExist {
		klog.Errorf("run: backup failed: [%v]", errors.New("no snapshot file exists"))
	}

	src := "/var/lib/etcd/member/snap"
	dst := "/var/backup/etcd/snap"
	if _, err := exec.Command("cp", getCpArgs(src, dst)...).CombinedOutput(); err != nil {
		klog.Errorf("run: backup failed: [%v]", err)
	}

	return nil
}

func getCpArgs(src, dst string) []string {
	return strings.Split(fmt.Sprintf("--verbose --recursive --preserve --reflink=auto %s %s", src, dst), " ")
}

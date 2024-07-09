package backuprestore

import (
	"context"
	"errors"
	"fmt"
	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	configversionedclientv1alpha1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	"io"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"k8s.io/klog/v2"
)

type backupNoConfig struct {
	kubeConfig    string
	snapshotExist bool
	schedule      string
	retention     backupv1alpha1.RetentionPolicy
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
	if err := b.extractBackupConfigs(); err != nil {
		return err
	}

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

func (b *backupNoConfig) extractBackupConfigs() error {
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", b.kubeConfig)
	if err != nil {
		bErr := fmt.Errorf("error loading kubeconfig: %v", err)
		klog.Error(bErr)
		return bErr
	}

	backupsClient, err := configversionedclientv1alpha1.NewForConfig(kubeConfig)
	if err != nil {
		bErr := fmt.Errorf("error creating etcd backups client: %v", err)
		klog.Error(bErr)
		return bErr
	}

	backups, err := backupsClient.Backups().List(context.Background(), v1.ListOptions{})
	if err != nil {
		lErr := fmt.Errorf("could not list backup CRDs, error was: [%v]", err)
		klog.Error(lErr)
		return lErr
	}

	idx := slices.IndexFunc(backups.Items, func(backup backupv1alpha1.Backup) bool {
		return backup.Name == "default"
	})
	if idx == -1 {
		sErr := fmt.Errorf("could not find default backup CR, found [%v]", backups.Items)
		klog.Error(sErr)
		return sErr
	}

	defaultBackupCR := backups.Items[idx]
	b.schedule = defaultBackupCR.Spec.EtcdBackupSpec.Schedule
	b.retention = defaultBackupCR.Spec.EtcdBackupSpec.RetentionPolicy

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

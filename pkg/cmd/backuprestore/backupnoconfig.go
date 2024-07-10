package backuprestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	backupv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	gcron "github.com/go-co-op/gocron/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	backupsClient, err := b.getBackupClient()
	if err != nil {
		return err
	}

	if err = b.extractBackupSpecs(backupsClient); err != nil {
		return err
	}

	var errs []error
	go func() {
		err := b.scheduleBackup()
		if err != nil {
			errs = append(errs, err)
		}
	}()
	go func() {
		err := b.scheduleBackupPrune()
		if err != nil {
			errs = append(errs, err)
		}
	}()

	if len(errs) > 0 {
		return kerrors.NewAggregate(errs)
	}
	return nil
}

func (b *backupNoConfig) getBackupClient() (backupv1client.BackupsGetter, error) {
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", b.kubeConfig)
	if err != nil {
		bErr := fmt.Errorf("error loading kubeconfig: %v", err)
		klog.Error(bErr)
		return nil, bErr
	}

	backupsClient, err := backupv1client.NewForConfig(kubeConfig)
	if err != nil {
		bErr := fmt.Errorf("error creating etcd backups client: %v", err)
		klog.Error(bErr)
		return nil, bErr
	}

	return backupsClient, nil
}

func (b *backupNoConfig) extractBackupSpecs(backupsClient backupv1client.BackupsGetter) error {
	backups, err := backupsClient.Backups().List(context.Background(), v1.ListOptions{})
	if err != nil {
		lErr := fmt.Errorf("could not list backup CRDs, error was: [%v]", err)
		klog.Error(lErr)
		return lErr
	}

	if len(backups.Items) == 0 {
		lErr := fmt.Errorf("no backup CRDs exist, found [%v]", backups)
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

func (b *backupNoConfig) backup() error {
	// initially take backup using etcdctl
	if !b.snapshotExist {
		if err := backup(&b.backupOptions); err != nil {
			klog.Errorf("run: backup failed: [%v]", err)
			return err
		}
		b.snapshotExist = true
		klog.Infof("config-dir is: %s", b.configDir)
		return nil
	}

	// only update the snapshot file
	if err := b.copySnapshot(); err != nil {
		sErr := fmt.Errorf("run: backup failed: [%v]", err)
		klog.Error(sErr)
		return sErr
	}

	return nil
}

func (b *backupNoConfig) scheduleBackup() error {
	s, _ := gcron.NewScheduler()
	defer func() { _ = s.Shutdown() }()

	if _, err := s.NewJob(
		gcron.CronJob(
			b.schedule,
			false,
		),
		gcron.NewTask(b.backup()),
	); err != nil {
		return err
	}

	s.Start()
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

func (b *backupNoConfig) pruneBackups() error {
	switch b.retention.RetentionType {
	case prune.RetentionTypeNone:
		klog.Info("no retention policy specified")
		return nil
	case prune.RetentionTypeNumber:
		if b.retention.RetentionNumber == nil {
			err := fmt.Errorf("retention policy RetentionTypeNumber requires RetentionNumberConfig")
			klog.Error(err)
			return err
		}
		return prune.Retain(b.retention)
	case prune.RetentionTypeSize:
		if b.retention.RetentionSize == nil {
			err := fmt.Errorf("retention policy RetentionTypeSize requires RetentionSizeConfig")
			klog.Error(err)
			return err
		}
		return prune.Retain(b.retention)
	default:
		err := fmt.Errorf("illegal retention policy type: [%v]", b.retention.RetentionType)
		klog.Error(err)
		return err
	}
}

func (b *backupNoConfig) scheduleBackupPrune() error {
	s, _ := gcron.NewScheduler()
	defer func() { _ = s.Shutdown() }()

	if _, err := s.NewJob(
		gcron.CronJob(
			b.schedule,
			false,
		),
		gcron.NewTask(b.pruneBackups()),
	); err != nil {
		return err
	}

	s.Start()
	return nil
}

func getCpArgs(src, dst string) []string {
	return strings.Split(fmt.Sprintf("--verbose --recursive --preserve --reflink=auto %s %s", src, dst), " ")
}

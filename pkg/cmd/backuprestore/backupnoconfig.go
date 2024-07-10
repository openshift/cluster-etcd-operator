package backuprestore

import (
	"context"
	"fmt"
	"io"
	"slices"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	backupv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	prune_backups "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	gcron "github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type backupNoConfig struct {
	kubeConfig    string
	snapshotExist bool
	schedule      string
	retention     backupv1alpha1.RetentionPolicy
	scheduler     gcron.Scheduler
	backupOptions
}

func NewBackupNoConfigCommand(errOut io.Writer) *cobra.Command {
	backupNoConf := &backupNoConfig{
		snapshotExist: false,
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

	b.scheduler, _ = gcron.NewScheduler(
		gcron.WithLimitConcurrentJobs(1, gcron.LimitModeWait),
		gcron.WithGlobalJobOptions(
			gcron.WithLimitedRuns(1),
			gcron.WithSingletonMode(gcron.LimitModeWait)))
	defer func() { _ = b.scheduler.Shutdown() }()

	if err = b.extractBackupSpecs(backupsClient); err != nil {
		return err
	}

	err = b.scheduleBackup()
	if err != nil {
		return err
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
	return backup(&b.backupOptions)
}

func (b *backupNoConfig) scheduleBackup() error {
	if _, err := b.scheduler.NewJob(
		gcron.CronJob(
			b.schedule,
			false,
		),
		gcron.NewTask(b.backup()),
		gcron.WithEventListeners(
			gcron.AfterJobRuns(
				func(jobID uuid.UUID, jobName string) {
					b.pruneBackups()
				},
			),
		),
	); err != nil {
		return err
	}

	b.scheduler.Start()
	return nil
}

func (b *backupNoConfig) pruneBackups() error {
	opts := &prune_backups.PruneOpts{
		RetentionType:      string(b.retention.RetentionType),
		MaxNumberOfBackups: b.retention.RetentionNumber.MaxNumberOfBackups,
		MaxSizeOfBackupsGb: b.retention.RetentionSize.MaxSizeOfBackupsGb,
	}

	if err := opts.Validate(); err != nil {
		return err
	}

	return opts.Run()
}

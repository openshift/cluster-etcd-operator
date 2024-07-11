package backuprestore

import (
	"context"
	"fmt"
	"io"
	"slices"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	backupv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	prunebackups "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/adhocore/gronx/pkg/tasker"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type backupNoConfig struct {
	kubeConfig string
	schedule   string
	timeZone   string
	retention  backupv1alpha1.RetentionPolicy
	scheduler  *tasker.Tasker
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

	b.scheduler = tasker.New(tasker.Option{
		Verbose: true,
		Tz:      b.timeZone,
	})

	err = b.scheduleBackup()
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
	b.timeZone = defaultBackupCR.Spec.EtcdBackupSpec.TimeZone

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

func (b *backupNoConfig) pruneBackups() error {
	opts := &prunebackups.PruneOpts{
		RetentionType:      string(b.retention.RetentionType),
		MaxNumberOfBackups: b.retention.RetentionNumber.MaxNumberOfBackups,
		MaxSizeOfBackupsGb: b.retention.RetentionSize.MaxSizeOfBackupsGb,
	}

	if err := opts.Validate(); err != nil {
		return err
	}

	return opts.Run()
}

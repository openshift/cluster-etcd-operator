package e2e

import (
	"context"
	"testing"
	"time"

	configv1alpha1 "github.com/openshift/api/config/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/openshift/library-go/test/library"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	defaultBackupCRName = "default"
	testSchedule        = "*/5 * * * *"
	testTimeZone        = "UTC"
	backupHostPath      = "/var/backup/etcd"
)

func TestEtcdBackupServer(t *testing.T) {
	configClient := framework.NewConfigClient(t)
	clientSet := framework.NewClientSet("")
	etcdPodsClient := clientSet.CoreV1Interface.Pods(operandNameSpace)

	backupCR := createDefaultBackupCR(testSchedule, testTimeZone)
	_, err := configClient.Backups().Create(context.Background(), backupCR, metav1.CreateOptions{})
	require.NoError(t, err)

	err = library.WaitForPodsToStabilizeOnTheSameRevision(t, etcdPodsClient, etcdLabelSelector, 5, 1*time.Minute, 5*time.Second, 30*time.Minute)
	require.NoError(t, err)

	masterNodes, err := clientSet.CoreV1Interface.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: masterNodeLabel})
	require.NoErrorf(t, err, "error listing master nodes")

	filesPerNode := map[string][]string{}
	for _, node := range masterNodes.Items {
		files := listFilesInVolume(t, backupHostPath, false, 30*time.Minute, node)
		filesPerNode[node.Name] = files
	}

	assertBackupFiles(t, filesPerNode)

	t.Cleanup(func() {
		cErr := configClient.Backups().Delete(context.Background(), backupCR.Name, metav1.DeleteOptions{})
		require.NoError(t, cErr)
	})
}

func createDefaultBackupCR(schedule, timeZone string) *configv1alpha1.Backup {
	return &configv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name: defaultBackupCRName,
		},
		Spec: configv1alpha1.BackupSpec{
			EtcdBackupSpec: configv1alpha1.EtcdBackupSpec{
				Schedule: schedule,
				TimeZone: timeZone,
				RetentionPolicy: configv1alpha1.RetentionPolicy{
					RetentionType: configv1alpha1.RetentionTypeNumber,
				},
			},
		},
	}
}

func assertBackupFiles(t *testing.T, filesMap map[string][]string) {
	for _, files := range filesMap {
		require.Contains(t, files[2], "snapshot_")
		require.Contains(t, files[3], "static_kuberesources_")
	}
}

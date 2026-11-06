package backuphelpers

import (
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestAutoBackupFeatureGateEnabled(t *testing.T) {
	backupFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{AutomatedEtcdBackupFeatureGateName},
		[]configv1.FeatureGateName{})
	enabled, err := AutoBackupFeatureGateEnabled(backupFeatureGateAccessor)
	require.Truef(t, enabled, "backup enabled = true")
	require.NoError(t, err)
}

func TestAutoBackupFeatureGateDisabled(t *testing.T) {
	backupFeatureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(
		[]configv1.FeatureGateName{},
		[]configv1.FeatureGateName{AutomatedEtcdBackupFeatureGateName})
	enabled, err := AutoBackupFeatureGateEnabled(backupFeatureGateAccessor)
	require.Falsef(t, enabled, "backup enabled = false")
	require.NoError(t, err)
}

func TestListBackupNodesSorted(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, name := range []string{"node-c", "node-a", "worker", "node-b"} {
		labels := map[string]string{ControlPlaneNodeLabelSelector: ""}
		if name == "worker" {
			labels = map[string]string{}
		}
		require.NoError(t, indexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}))
	}
	nodes, err := ListBackupNodes(corev1listers.NewNodeLister(indexer), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}
	require.Equal(t, []string{"node-a", "node-b", "node-c"}, names)
}

func TestLocalBackupHostPath(t *testing.T) {
	require.Equal(t, BackupPathMount, LocalBackupHostPath(operatorv1alpha1.EtcdBackupStorageLocal{}))
	require.Equal(t, "/backups", LocalBackupHostPath(operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/backups/"}))
	require.Equal(t, "/backups", LocalBackupHostPath(operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/var/../backups"}))
}

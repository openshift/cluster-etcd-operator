package scriptcontroller

import (
	"context"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

func getTestController(t *testing.T, topology configv1.TopologyMode, envVars map[string]string) (*ScriptController, *fake.Clientset) {
	fakeKubeClient := fake.NewSimpleClientset()
	infraLister := testutils.FakeInfrastructureLister(t, topology)
	fakeEnvVarGetter := &etcdenvvar.FakeEnvVar{
		EnvVars: envVars,
	}

	controller := &ScriptController{
		kubeClient:   fakeKubeClient,
		infraLister:  infraLister,
		envVarGetter: fakeEnvVarGetter,
		enqueueFn:    func() {},
	}

	return controller, fakeKubeClient
}

func TestManageScriptConfigMap(t *testing.T) {
	testCases := []struct {
		name                     string
		topology                 configv1.TopologyMode
		expectedRestoreContent   string
		expectedDisableContent   string
		unexpectedRestoreContent string
		unexpectedDisableContent string
	}{
		{
			name:                     "TNF topology deploys TNF-specific scripts",
			topology:                 configv1.DualReplicaTopologyMode,
			expectedRestoreContent:   "pcs resource enable etcd",
			expectedDisableContent:   "pcs resource disable etcd",
			unexpectedRestoreContent: "wait_for_containers_to_stop",
			unexpectedDisableContent: "restore_static_pods",
		},
		{
			name:                     "HighlyAvailable topology deploys standard scripts",
			topology:                 configv1.HighlyAvailableTopologyMode,
			expectedRestoreContent:   "wait_for_containers_to_stop",
			expectedDisableContent:   "mv_static_pods",
			unexpectedRestoreContent: "pcs resource disable etcd",
			unexpectedDisableContent: "podman container exists",
		},
		{
			name:                     "SingleReplica topology deploys standard scripts",
			topology:                 configv1.SingleReplicaTopologyMode,
			expectedRestoreContent:   "wait_for_containers_to_stop",
			expectedDisableContent:   "mv_static_pods",
			unexpectedRestoreContent: "crm_attribute",
			unexpectedDisableContent: "ensure_etcd_container_stopped",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Set up controller with test dependencies
			envVars := map[string]string{
				"ETCD_IMAGE": "quay.io/openshift/etcd:latest",
			}
			controller, fakeKubeClient := getTestController(t, tc.topology, envVars)

			// Call the function under test
			recorder := events.NewInMemoryRecorder("test", clocktesting.NewFakePassiveClock(time.Now()))
			configMap, _, err := controller.manageScriptConfigMap(context.Background(), recorder)

			// Verify no errors
			require.NoError(t, err)
			require.NotNil(t, configMap)

			// Verify ConfigMap was created in the cluster
			scriptCM, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(
				context.Background(), "etcd-scripts", metav1.GetOptions{})
			require.NoError(t, err)

			// Verify cluster-restore.sh contains expected content
			require.Contains(t, scriptCM.Data, "cluster-restore.sh", "cluster-restore.sh should be in ConfigMap")
			require.Contains(t, scriptCM.Data["cluster-restore.sh"], tc.expectedRestoreContent,
				"cluster-restore.sh should contain topology-specific content")
			require.NotContains(t, scriptCM.Data["cluster-restore.sh"], tc.unexpectedRestoreContent,
				"cluster-restore.sh should not contain wrong topology content")

			// Verify disable-etcd.sh contains expected content
			require.Contains(t, scriptCM.Data, "disable-etcd.sh", "disable-etcd.sh should be in ConfigMap")
			require.Contains(t, scriptCM.Data["disable-etcd.sh"], tc.expectedDisableContent,
				"disable-etcd.sh should contain topology-specific content")
			require.NotContains(t, scriptCM.Data["disable-etcd.sh"], tc.unexpectedDisableContent,
				"disable-etcd.sh should not contain wrong topology content")

			// Verify common scripts are always present
			require.Contains(t, scriptCM.Data, "quorum-restore.sh", "quorum-restore.sh should be in ConfigMap")
			require.Contains(t, scriptCM.Data, "cluster-backup.sh", "cluster-backup.sh should be in ConfigMap")
			require.Contains(t, scriptCM.Data, "etcd-common-tools", "etcd-common-tools should be in ConfigMap")
			require.Contains(t, scriptCM.Data, "etcd.env", "etcd.env should be in ConfigMap")

			// Verify etcd.env has correct content
			require.Contains(t, scriptCM.Data["etcd.env"], "ETCD_IMAGE", "etcd.env should contain environment variables")
		})
	}
}

func TestManageScriptConfigMap_MissingEnvVars(t *testing.T) {
	// Set up controller with empty env vars (should cause error)
	envVars := map[string]string{}
	controller, _ := getTestController(t, configv1.HighlyAvailableTopologyMode, envVars)

	// Call the function under test
	recorder := events.NewInMemoryRecorder("test", clocktesting.NewFakePassiveClock(time.Now()))
	_, _, err := controller.manageScriptConfigMap(context.Background(), recorder)

	// Verify error is returned
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing env var values")
}

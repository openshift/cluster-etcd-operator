package backuppolicycontroller

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/metrics"
)

func TestBackupPolicyMetricsMissingStateEmitsLastSuccessZero(t *testing.T) {
	createdAt := metav1.NewTime(time.Unix(1700000000, 0))
	policy := testutils.FakeEtcdBackupPolicy("daily", "0 1 * * *", func(p *operatorv1alpha1.EtcdBackupPolicy) {
		p.CreationTimestamp = createdAt
	})

	collector := setupPolicyCollector(t, []*operatorv1alpha1.EtcdBackupPolicy{policy}, nil)

	expectedLastSuccess := `
		# HELP etcd_backup_policy_last_success_time Unix timestamp of last successful backup run (0 if never succeeded or tracking unknown)
		# TYPE etcd_backup_policy_last_success_time gauge
		etcd_backup_policy_last_success_time{policy="daily",uid="daily-uid"} 0
	`
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedLastSuccess), backupPolicyLastSuccessTimeMetricName))

	expectedCreation := fmt.Sprintf(`
		# HELP etcd_backup_policy_creation_time Unix timestamp when policy was created
		# TYPE etcd_backup_policy_creation_time gauge
		etcd_backup_policy_creation_time{policy="daily",uid="daily-uid"} %d
	`, createdAt.Unix())
	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(expectedCreation), backupPolicyCreationTimeMetricName))

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`
		# HELP etcd_backup_policy_tracker_error Whether reading backup failure tracker state failed (1) or succeeded (0). When 1, last_success_time may be unknown and is emitted as 0.
		# TYPE etcd_backup_policy_tracker_error gauge
		etcd_backup_policy_tracker_error 0
	`), backupPolicyTrackerErrorMetricName))
}

func TestBackupPolicyMetricsCorruptTrackerEmitsErrorAndLastSuccessZero(t *testing.T) {
	policy := testutils.FakeEtcdBackupPolicy("daily", "0 1 * * *")
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: failureTrackerConfigMapName, Namespace: "openshift-etcd-operator"},
		Data:       map[string]string{failureTrackerDataKey: "invalid json {"},
	}

	collector := setupPolicyCollector(t, []*operatorv1alpha1.EtcdBackupPolicy{policy}, cm)

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`
		# HELP etcd_backup_policy_tracker_error Whether reading backup failure tracker state failed (1) or succeeded (0). When 1, last_success_time may be unknown and is emitted as 0.
		# TYPE etcd_backup_policy_tracker_error gauge
		etcd_backup_policy_tracker_error 1
	`), backupPolicyTrackerErrorMetricName))

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`
		# HELP etcd_backup_policy_last_success_time Unix timestamp of last successful backup run (0 if never succeeded or tracking unknown)
		# TYPE etcd_backup_policy_last_success_time gauge
		etcd_backup_policy_last_success_time{policy="daily",uid="daily-uid"} 0
	`), backupPolicyLastSuccessTimeMetricName))
}

func TestBackupPolicyMetricsWithState(t *testing.T) {
	successAt := metav1.NewTime(time.Unix(1700001000, 0))
	createdAt := metav1.NewTime(time.Unix(1700000000, 0))
	policy := testutils.FakeEtcdBackupPolicy("daily", "0 1 * * *")

	states := map[string]*PolicyRunState{
		string(policy.UID): {
			PolicyUID:           policy.UID,
			ConsecutiveFailures: 2,
			LastSuccessTime:     &successAt,
			PolicyCreationTime:  createdAt,
		},
	}
	data, err := json.Marshal(states)
	require.NoError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: failureTrackerConfigMapName, Namespace: "openshift-etcd-operator"},
		Data:       map[string]string{failureTrackerDataKey: string(data)},
	}

	collector := setupPolicyCollector(t, []*operatorv1alpha1.EtcdBackupPolicy{policy}, cm)

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`
		# HELP etcd_backup_policy_tracker_error Whether reading backup failure tracker state failed (1) or succeeded (0). When 1, last_success_time may be unknown and is emitted as 0.
		# TYPE etcd_backup_policy_tracker_error gauge
		etcd_backup_policy_tracker_error 0
	`), backupPolicyTrackerErrorMetricName))

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(fmt.Sprintf(`
		# HELP etcd_backup_policy_last_success_time Unix timestamp of last successful backup run (0 if never succeeded or tracking unknown)
		# TYPE etcd_backup_policy_last_success_time gauge
		etcd_backup_policy_last_success_time{policy="daily",uid="daily-uid"} %d
	`, successAt.Unix())), backupPolicyLastSuccessTimeMetricName))

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`
		# HELP etcd_backup_policy_consecutive_failures Number of consecutive backup run failures since last success
		# TYPE etcd_backup_policy_consecutive_failures gauge
		etcd_backup_policy_consecutive_failures{policy="daily",uid="daily-uid"} 2
	`), backupPolicyConsecutiveFailuresMetricName))
}

func TestParseAllPolicyStates(t *testing.T) {
	t.Run("nil-configmap", func(t *testing.T) {
		states, err := parseAllPolicyStates(nil)
		require.NoError(t, err)
		require.Empty(t, states)
	})
	t.Run("missing-key", func(t *testing.T) {
		states, err := parseAllPolicyStates(&corev1.ConfigMap{Data: map[string]string{}})
		require.NoError(t, err)
		require.Empty(t, states)
	})
	t.Run("corrupt", func(t *testing.T) {
		_, err := parseAllPolicyStates(&corev1.ConfigMap{
			Data: map[string]string{failureTrackerDataKey: "{"},
		})
		require.Error(t, err)
	})
}

func setupPolicyCollector(t *testing.T, policies []*operatorv1alpha1.EtcdBackupPolicy, trackerCM *corev1.ConfigMap) prometheus.Collector {
	t.Helper()

	policyObjs := make([]runtime.Object, 0, len(policies))
	policyObjs = testutils.AppendRuntimeObjects(policyObjs, policies)
	policyClient := operatorfake.NewSimpleClientset(policyObjs...)
	policyInformers := operatorinformers.NewSharedInformerFactory(policyClient, 0)
	policyInformer := policyInformers.Operator().V1alpha1().EtcdBackupPolicies()
	for _, policy := range policies {
		require.NoError(t, policyInformer.Informer().GetIndexer().Add(policy))
	}

	var cmObjs []runtime.Object
	if trackerCM != nil {
		cmObjs = append(cmObjs, trackerCM)
	}
	kubeClient := k8sfake.NewSimpleClientset(cmObjs...)
	kubeInformers := informers.NewSharedInformerFactory(kubeClient, 0)
	cmInformer := kubeInformers.Core().V1().ConfigMaps()
	if trackerCM != nil {
		require.NoError(t, cmInformer.Informer().GetIndexer().Add(trackerCM))
	}

	return NewBackupPolicyMetrics(
		metrics.NewKubeRegistry(),
		policyInformer.Lister(),
		cmInformer.Lister().ConfigMaps("openshift-etcd-operator"),
		backupFeatureGateAccessor,
	)
}

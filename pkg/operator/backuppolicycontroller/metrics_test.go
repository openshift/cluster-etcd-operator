package backuppolicycontroller

import (
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-base/metrics"
)

func TestBackupPolicyMetrics(t *testing.T) {
	createdAt := metav1.NewTime(time.Unix(1700000000, 0))
	policy := testutils.FakeEtcdBackupPolicy("daily", "0 1 * * *", func(p *operatorv1alpha1.EtcdBackupPolicy) {
		p.CreationTimestamp = createdAt
	})

	collector := setupPolicyCollector(t, []*operatorv1alpha1.EtcdBackupPolicy{policy})

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(fmt.Sprintf(`
		# HELP etcd_backup_policy_creation_timestamp_seconds Unix timestamp when policy was created
		# TYPE etcd_backup_policy_creation_timestamp_seconds gauge
		etcd_backup_policy_creation_timestamp_seconds{etcd_backup_policy="daily",uid="daily-uid"} %d
	`, createdAt.Unix())), backupPolicyCreationTimeMetricName))

	require.NoError(t, testutil.CollectAndCompare(collector, strings.NewReader(`
		# HELP etcd_backup_policy_schedule_interval_seconds Estimated maximum seconds between scheduled backup runs for the policy. Used by freshness alerts as the schedule interval; omitted when the schedule cannot be parsed.
		# TYPE etcd_backup_policy_schedule_interval_seconds gauge
		etcd_backup_policy_schedule_interval_seconds{etcd_backup_policy="daily",uid="daily-uid"} 86400
	`), backupPolicyScheduleIntervalSecondsMetricName))
}

func TestBackupPolicyMetricsInvalidScheduleOmitsInterval(t *testing.T) {
	policy := testutils.FakeEtcdBackupPolicy("broken", "not a cron")
	collector := setupPolicyCollector(t, []*operatorv1alpha1.EtcdBackupPolicy{policy})

	count := testutil.CollectAndCount(collector, backupPolicyScheduleIntervalSecondsMetricName)
	require.Equal(t, 0, count, "unparseable schedules must omit interval metric")
}

func setupPolicyCollector(t *testing.T, policies []*operatorv1alpha1.EtcdBackupPolicy) prometheus.Collector {
	t.Helper()

	policyObjs := make([]runtime.Object, 0, len(policies))
	policyObjs = testutils.AppendRuntimeObjects(policyObjs, policies)
	policyClient := operatorfake.NewSimpleClientset(policyObjs...)
	policyInformers := operatorinformers.NewSharedInformerFactory(policyClient, 0)
	policyInformer := policyInformers.Operator().V1alpha1().EtcdBackupPolicies()
	for _, policy := range policies {
		require.NoError(t, policyInformer.Informer().GetIndexer().Add(policy))
	}

	return NewBackupPolicyMetrics(
		metrics.NewKubeRegistry(),
		policyInformer.Lister(),
	)
}

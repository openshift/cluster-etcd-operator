package backuppolicycontroller

import (
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

const (
	backupPolicyInfoMetricName                    = "etcd_backup_policy_info"
	backupPolicyCreationTimeMetricName            = "etcd_backup_policy_creation_time"
	backupPolicyScheduleIntervalSecondsMetricName = "etcd_backup_policy_schedule_interval_seconds"

	storageTypePVC   = "PVC"
	storageTypeLocal = "Local"
)

// policyCollector implements prometheus.Collector and generates metrics
// on-demand at scrape time from the cached EtcdBackupPolicy lister.
type policyCollector struct {
	policiesLister operatorv1alpha1listers.EtcdBackupPolicyLister

	infoDesc             *prometheus.Desc
	creationTimeDesc     *prometheus.Desc
	scheduleIntervalDesc *prometheus.Desc
}

// NewBackupPolicyMetrics creates and registers a policy collector with the provided registry.
func NewBackupPolicyMetrics(
	metricsRegistry metrics.KubeRegistry,
	policiesLister operatorv1alpha1listers.EtcdBackupPolicyLister,
) *policyCollector {
	c := &policyCollector{
		policiesLister: policiesLister,
		infoDesc: prometheus.NewDesc(
			backupPolicyInfoMetricName,
			"Information about etcd backup policy",
			[]string{"policy", "uid", "schedule", "timezone", "storage_type", "storage_location"},
			nil,
		),
		creationTimeDesc: prometheus.NewDesc(
			backupPolicyCreationTimeMetricName,
			"Unix timestamp when policy was created",
			[]string{"policy", "uid"},
			nil,
		),
		scheduleIntervalDesc: prometheus.NewDesc(
			backupPolicyScheduleIntervalSecondsMetricName,
			"Estimated maximum seconds between scheduled backup runs for the policy. Used by freshness alerts as the schedule interval; omitted when the schedule cannot be parsed.",
			[]string{"policy", "uid"},
			nil,
		),
	}

	metricsRegistry.RawMustRegister(c)
	return c
}

// Describe implements prometheus.Collector
func (c *policyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.infoDesc
	ch <- c.creationTimeDesc
	ch <- c.scheduleIntervalDesc
}

// Collect implements prometheus.Collector
func (c *policyCollector) Collect(ch chan<- prometheus.Metric) {
	policies, err := c.policiesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("policyCollector failed to list backup policies: %v", err)
		return
	}

	for _, policy := range policies {
		c.collectPolicyMetrics(ch, policy)
	}
}

func (c *policyCollector) collectPolicyMetrics(ch chan<- prometheus.Metric, policy *operatorv1alpha1.EtcdBackupPolicy) {
	name := policy.Name
	uid := string(policy.UID)
	schedule := policy.Spec.Schedule
	timezone := policy.Spec.TimeZone
	if timezone == "" {
		timezone = "UTC"
	}
	storageType, storageLocation := extractPolicyStorageInfo(policy)

	ch <- prometheus.MustNewConstMetric(
		c.infoDesc,
		prometheus.GaugeValue,
		1,
		name, uid, schedule, timezone, storageType, storageLocation,
	)
	ch <- prometheus.MustNewConstMetric(
		c.creationTimeDesc,
		prometheus.GaugeValue,
		float64(policy.CreationTimestamp.Unix()),
		name, uid,
	)
	if intervalSeconds, ok := estimateScheduleIntervalSeconds(schedule); ok {
		ch <- prometheus.MustNewConstMetric(
			c.scheduleIntervalDesc,
			prometheus.GaugeValue,
			intervalSeconds,
			name, uid,
		)
	}
}

func extractPolicyStorageInfo(policy *operatorv1alpha1.EtcdBackupPolicy) (string, string) {
	var storageType, storageLocation string

	switch policy.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		storageType = storageTypePVC
		storageLocation = policy.Spec.Storage.PVC.Name
		if policy.Spec.Storage.PVC.Path != "" {
			storageLocation = policy.Spec.Storage.PVC.Name + ":" + policy.Spec.Storage.PVC.Path
		}
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		storageType = storageTypeLocal
		storageLocation = policy.Spec.Storage.Local.HostPath
	}

	return storageType, storageLocation
}

// MustRegisterDefaultBackupPolicyMetrics creates policy metrics using the default registry.
// Deprecated: Use NewBackupPolicyMetrics with an explicit registry for production code.
func MustRegisterDefaultBackupPolicyMetrics(
	policiesLister operatorv1alpha1listers.EtcdBackupPolicyLister,
) *policyCollector {
	return NewBackupPolicyMetrics(legacyregistry.DefaultGatherer.(metrics.KubeRegistry), policiesLister)
}

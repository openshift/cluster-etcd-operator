package backuppolicycontroller

import (
	"context"
	"path/filepath"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

const (
	automatedBackupEnabledMetricName          = "etcd_automated_backup_enabled"
	backupPolicyInfoMetricName                = "etcd_backup_policy_info"
	backupPolicyConsecutiveFailuresMetricName = "etcd_backup_policy_consecutive_failures"
	backupPolicyLastSuccessTimeMetricName     = "etcd_backup_policy_last_success_time"
	backupPolicyCreationTimeMetricName        = "etcd_backup_policy_creation_time"

	storageTypePVC   = "PVC"
	storageTypeLocal = "Local"
)

// policyCollector implements prometheus.Collector and generates metrics
// on-demand at scrape time by reading current EtcdBackupPolicy objects and RunTracker state.
type policyCollector struct {
	policiesLister      operatorv1alpha1listers.EtcdBackupPolicyLister
	runTracker          *RunTracker
	featureGateAccessor featuregates.FeatureGateAccess

	enabledDesc             *prometheus.Desc
	infoDesc                *prometheus.Desc
	consecutiveFailuresDesc *prometheus.Desc
	lastSuccessTimeDesc     *prometheus.Desc
	creationTimeDesc        *prometheus.Desc
}

// NewBackupPolicyMetrics creates and registers a policy collector with the provided registry.
// The collector dynamically generates metrics at scrape time from the EtcdBackupPolicyLister
// and RunTracker state.
func NewBackupPolicyMetrics(metricsRegistry metrics.KubeRegistry, policiesLister operatorv1alpha1listers.EtcdBackupPolicyLister, runTracker *RunTracker, featureGateAccessor featuregates.FeatureGateAccess) *policyCollector {
	c := &policyCollector{
		policiesLister:      policiesLister,
		runTracker:          runTracker,
		featureGateAccessor: featureGateAccessor,
		enabledDesc: prometheus.NewDesc(
			automatedBackupEnabledMetricName,
			"Whether AutomatedEtcdBackup feature gate is enabled (1) or disabled (0)",
			nil,
			nil,
		),
		infoDesc: prometheus.NewDesc(
			backupPolicyInfoMetricName,
			"Information about etcd backup policy",
			[]string{"policy", "uid", "schedule", "timezone", "storage_type", "storage_location"},
			nil,
		),
		consecutiveFailuresDesc: prometheus.NewDesc(
			backupPolicyConsecutiveFailuresMetricName,
			"Number of consecutive backup run failures since last success",
			[]string{"policy", "uid"},
			nil,
		),
		lastSuccessTimeDesc: prometheus.NewDesc(
			backupPolicyLastSuccessTimeMetricName,
			"Unix timestamp of last successful backup run (0 if never succeeded)",
			[]string{"policy", "uid"},
			nil,
		),
		creationTimeDesc: prometheus.NewDesc(
			backupPolicyCreationTimeMetricName,
			"Unix timestamp when policy was created",
			[]string{"policy", "uid"},
			nil,
		),
	}

	metricsRegistry.RawMustRegister(c)
	return c
}

// Describe implements prometheus.Collector
func (c *policyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.enabledDesc
	ch <- c.infoDesc
	ch <- c.consecutiveFailuresDesc
	ch <- c.lastSuccessTimeDesc
	ch <- c.creationTimeDesc
}

// Collect implements prometheus.Collector
func (c *policyCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	// Emit feature gate enabled metric
	enabled := float64(0)
	if isEnabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); err == nil && isEnabled {
		enabled = 1
	}
	ch <- prometheus.MustNewConstMetric(
		c.enabledDesc,
		prometheus.GaugeValue,
		enabled,
	)

	// Get all policies
	policies, err := c.policiesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("policyCollector failed to list backup policies: %v", err)
		return
	}

	// Get all run states from RunTracker
	allStates, err := c.runTracker.GetAllPolicyStates(ctx)
	if err != nil {
		klog.Errorf("policyCollector failed to get policy states: %v", err)
		allStates = make(map[string]*PolicyRunState) // Continue with empty states
	}

	for _, policy := range policies {
		c.collectPolicyMetrics(ch, policy, allStates)
	}
}

func (c *policyCollector) collectPolicyMetrics(ch chan<- prometheus.Metric, policy *operatorv1alpha1.EtcdBackupPolicy, allStates map[string]*PolicyRunState) {
	name := policy.Name
	uid := string(policy.UID)
	schedule := policy.Spec.Schedule
	timezone := "UTC"
	storageType, storageLocation := extractPolicyStorageInfo(policy)

	// Emit info metric
	ch <- prometheus.MustNewConstMetric(
		c.infoDesc,
		prometheus.GaugeValue,
		1,
		name, uid, schedule, timezone, storageType, storageLocation,
	)

	// Get state for this policy
	state, hasState := allStates[uid]
	if !hasState {
		// Policy exists but no state yet (new policy, never synced)
		// Still emit creation time
		ch <- prometheus.MustNewConstMetric(
			c.creationTimeDesc,
			prometheus.GaugeValue,
			float64(policy.CreationTimestamp.Unix()),
			name, uid,
		)
		return
	}

	// Emit consecutive failures (only if > 0)
	if state.ConsecutiveFailures > 0 {
		ch <- prometheus.MustNewConstMetric(
			c.consecutiveFailuresDesc,
			prometheus.GaugeValue,
			float64(state.ConsecutiveFailures),
			name, uid,
		)
	}

	// Emit last success time (0 if never succeeded)
	lastSuccessTime := float64(0)
	if state.LastSuccessTime != nil {
		lastSuccessTime = float64(state.LastSuccessTime.Unix())
	}
	ch <- prometheus.MustNewConstMetric(
		c.lastSuccessTimeDesc,
		prometheus.GaugeValue,
		lastSuccessTime,
		name, uid,
	)

	// Emit creation time
	ch <- prometheus.MustNewConstMetric(
		c.creationTimeDesc,
		prometheus.GaugeValue,
		float64(state.PolicyCreationTime.Unix()),
		name, uid,
	)
}

func extractPolicyStorageInfo(policy *operatorv1alpha1.EtcdBackupPolicy) (string, string) {
	var storageType, storageLocation string

	switch policy.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		storageType = storageTypePVC
		storageLocation = policy.Spec.Storage.PVC.Name
		if policy.Spec.Storage.PVC.Path != "" {
			storageLocation = filepath.Join(policy.Spec.Storage.PVC.Name, policy.Spec.Storage.PVC.Path)
		}
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		storageType = storageTypeLocal
		storageLocation = policy.Spec.Storage.Local.HostPath

	}

	return storageType, storageLocation
}

// MustRegisterDefaultBackupPolicyMetrics creates policy metrics using the default registry.
// Deprecated: Use NewBackupPolicyMetrics with an explicit registry for production code.
// This function remains for test compatibility.
func MustRegisterDefaultBackupPolicyMetrics(policiesLister operatorv1alpha1listers.EtcdBackupPolicyLister, runTracker *RunTracker, featureGateAccessor featuregates.FeatureGateAccess) *policyCollector {
	return NewBackupPolicyMetrics(legacyregistry.DefaultGatherer.(metrics.KubeRegistry), policiesLister, runTracker, featureGateAccessor)
}

package backuppolicycontroller

import (
	"path/filepath"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
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
	backupPolicyTrackerErrorMetricName        = "etcd_backup_policy_tracker_error"

	storageTypePVC   = "PVC"
	storageTypeLocal = "Local"
)

// policyCollector implements prometheus.Collector and generates metrics
// on-demand at scrape time from cached EtcdBackupPolicy and ConfigMap listers.
type policyCollector struct {
	policiesLister      operatorv1alpha1listers.EtcdBackupPolicyLister
	trackerConfigMaps   corev1listers.ConfigMapNamespaceLister
	featureGateAccessor featuregates.FeatureGateAccess

	enabledDesc             *prometheus.Desc
	infoDesc                *prometheus.Desc
	consecutiveFailuresDesc *prometheus.Desc
	lastSuccessTimeDesc     *prometheus.Desc
	creationTimeDesc        *prometheus.Desc
	trackerErrorDesc        *prometheus.Desc
}

// NewBackupPolicyMetrics creates and registers a policy collector with the provided registry.
// Tracker state is read from the ConfigMap informer cache (no live API calls on scrape).
func NewBackupPolicyMetrics(
	metricsRegistry metrics.KubeRegistry,
	policiesLister operatorv1alpha1listers.EtcdBackupPolicyLister,
	trackerConfigMaps corev1listers.ConfigMapNamespaceLister,
	featureGateAccessor featuregates.FeatureGateAccess,
) *policyCollector {
	c := &policyCollector{
		policiesLister:      policiesLister,
		trackerConfigMaps:   trackerConfigMaps,
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
			"Unix timestamp of last successful backup run (0 if never succeeded). When etcd_backup_policy_tracker_error is 1 this value is unknown and also emitted as 0; alerts must gate on tracker_error.",
			[]string{"policy", "uid"},
			nil,
		),
		creationTimeDesc: prometheus.NewDesc(
			backupPolicyCreationTimeMetricName,
			"Unix timestamp when policy was created",
			[]string{"policy", "uid"},
			nil,
		),
		trackerErrorDesc: prometheus.NewDesc(
			backupPolicyTrackerErrorMetricName,
			"Whether reading backup failure tracker state failed (1) or succeeded (0). 1 covers corrupt JSON or a present ConfigMap missing tracker data (lost state). A missing ConfigMap is first initialization and yields 0.",
			nil,
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
	ch <- c.trackerErrorDesc
}

// Collect implements prometheus.Collector
func (c *policyCollector) Collect(ch chan<- prometheus.Metric) {
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

	policies, err := c.policiesLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("policyCollector failed to list backup policies: %v", err)
		ch <- prometheus.MustNewConstMetric(c.trackerErrorDesc, prometheus.GaugeValue, 1)
		return
	}

	allStates, trackerErr := c.loadTrackerStates()
	trackerErrorValue := float64(0)
	if trackerErr != nil {
		klog.Errorf("policyCollector failed to get policy states: %v", trackerErr)
		trackerErrorValue = 1
		allStates = make(map[string]*PolicyRunState)
	}
	ch <- prometheus.MustNewConstMetric(c.trackerErrorDesc, prometheus.GaugeValue, trackerErrorValue)

	for _, policy := range policies {
		c.collectPolicyMetrics(ch, policy, allStates)
	}
}

func (c *policyCollector) loadTrackerStates() (map[string]*PolicyRunState, error) {
	cm, err := c.trackerConfigMaps.Get(failureTrackerConfigMapName)
	if apierrors.IsNotFound(err) {
		return make(map[string]*PolicyRunState), nil
	}
	if err != nil {
		return nil, err
	}
	return parseAllPolicyStates(cm)
}

func (c *policyCollector) collectPolicyMetrics(ch chan<- prometheus.Metric, policy *operatorv1alpha1.EtcdBackupPolicy, allStates map[string]*PolicyRunState) {
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

	state, hasState := allStates[uid]

	// Always emit last_success_time so alert rules that require the series
	// (e.g. last_success_time == 0) remain evaluable when tracking is missing.
	lastSuccessTime := float64(0)
	creationTime := float64(policy.CreationTimestamp.Unix())
	if hasState {
		if state.LastSuccessTime != nil {
			lastSuccessTime = float64(state.LastSuccessTime.Unix())
		}
		if !state.PolicyCreationTime.IsZero() {
			creationTime = float64(state.PolicyCreationTime.Unix())
		}
		if state.ConsecutiveFailures > 0 {
			ch <- prometheus.MustNewConstMetric(
				c.consecutiveFailuresDesc,
				prometheus.GaugeValue,
				float64(state.ConsecutiveFailures),
				name, uid,
			)
		}
	}

	ch <- prometheus.MustNewConstMetric(
		c.lastSuccessTimeDesc,
		prometheus.GaugeValue,
		lastSuccessTime,
		name, uid,
	)
	ch <- prometheus.MustNewConstMetric(
		c.creationTimeDesc,
		prometheus.GaugeValue,
		creationTime,
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
	trackerConfigMaps corev1listers.ConfigMapNamespaceLister,
	featureGateAccessor featuregates.FeatureGateAccess,
) *policyCollector {
	return NewBackupPolicyMetrics(legacyregistry.DefaultGatherer.(metrics.KubeRegistry), policiesLister, trackerConfigMaps, featureGateAccessor)
}

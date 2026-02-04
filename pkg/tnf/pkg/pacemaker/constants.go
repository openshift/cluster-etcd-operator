package pacemaker

import "time"

// API constants - shared between healthcheck and statuscollector
const (
	// Kubernetes API path for custom resources
	KubernetesAPIPath = "/apis"

	// PacemakerCluster resource name (plural) for API calls
	PacemakerResourceName = "pacemakerclusters"

	// PacemakerClusterResourceName is the name of the singleton PacemakerCluster CR
	PacemakerClusterResourceName = "cluster"
)

// Resource agent identifiers - used to identify pacemaker resources by type
const (
	ResourceAgentKubelet  = "systemd:kubelet"
	ResourceAgentEtcd     = "ocf:heartbeat:podman-etcd"
	ResourceAgentFencing  = "stonith:"
	ResourceAgentFenceAWS = "fence_aws"
)

// Time windows and thresholds
//
// Timing Relationship:
//   - The status collector CronJob runs every 2 minutes (configured in cronjob.yaml)
//   - StatusStalenessThreshold (5 min) allows for ~2 missed collections before staleness
//   - StatusUnknownDegradedThreshold (5 min) provides grace period for transient issues
//   - HealthCheckResyncInterval (30s) ensures quick detection of CR updates
const (
	// HealthCheckResyncInterval is how often the health check controller syncs.
	// Set lower than the CronJob schedule to detect CR updates promptly.
	HealthCheckResyncInterval = 30 * time.Second

	// StatusStalenessThreshold is how long before status is considered stale.
	// Should be > 2x the CronJob schedule to allow for transient collection failures.
	StatusStalenessThreshold = 5 * time.Minute

	// StatusUnknownDegradedThreshold is how long to wait in unknown status before marking degraded.
	// Matches StatusStalenessThreshold to provide consistent grace period behavior.
	StatusUnknownDegradedThreshold = 5 * time.Minute

	// EventDeduplicationWindowFencing is the window for fencing event deduplication
	EventDeduplicationWindowFencing = 24 * time.Hour

	// EventDeduplicationWindowDefault is the default event deduplication window
	EventDeduplicationWindowDefault = 5 * time.Minute

	// FailedActionTimeWindow is the window for detecting recent failed actions
	FailedActionTimeWindow = 5 * time.Minute

	// FencingEventTimeWindow is the window for detecting recent fencing events
	FencingEventTimeWindow = 24 * time.Hour
)

// Cluster configuration
const (
	// ExpectedNodeCount is the expected number of nodes in a TNF cluster
	ExpectedNodeCount = 2

	// TargetNamespace is the namespace for events and resources
	TargetNamespace = "openshift-etcd"

	// StatusCollectorCronJobName is the name of the CronJob that runs the status collector
	StatusCollectorCronJobName = "pacemaker-status-collector"
)

// Event reasons - shared between healthcheck and statuscollector
const (
	// Event reasons for non-degrading conditions (informational/historical)
	EventReasonFailedAction    = "PacemakerFailedResourceAction"
	EventReasonFencingEvent    = "PacemakerFencingEvent"
	EventReasonWarning         = "PacemakerWarning"
	EventReasonHealthy         = "PacemakerHealthy"
	EventReasonWarningsCleared = "PacemakerWarningsCleared"

	// Event reasons for degrading conditions (current operational problems)
	EventReasonNodeOffline          = "PacemakerNodeOffline"
	EventReasonNodeUnhealthy        = "PacemakerNodeUnhealthy"
	EventReasonResourceUnhealthy    = "PacemakerResourceUnhealthy"
	EventReasonClusterUnhealthy     = "PacemakerClusterUnhealthy"
	EventReasonStatusStale          = "PacemakerStatusStale"
	EventReasonCRNotFound           = "PacemakerCRNotFound"
	EventReasonInsufficientNodes    = "PacemakerInsufficientNodes"
	EventReasonClusterInMaintenance = "PacemakerClusterInMaintenance"
	EventReasonGenericError         = "PacemakerError"

	// Event reasons for status collector
	EventReasonCollectionError = "PacemakerStatusCollectionError"
)

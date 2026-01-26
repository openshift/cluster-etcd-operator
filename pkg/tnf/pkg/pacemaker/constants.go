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
const (
	// HealthCheckResyncInterval is how often the health check controller syncs
	HealthCheckResyncInterval = 30 * time.Second

	// StatusStalenessThreshold is how long before status is considered stale
	StatusStalenessThreshold = 5 * time.Minute

	// StatusUnknownDegradedThreshold is how long to wait in unknown status before marking degraded
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

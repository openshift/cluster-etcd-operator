package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=pacemakerstatuses,scope=Cluster
// +kubebuilder:subresource:status

// PacemakerStatus represents the current state of the Pacemaker cluster
// as reported by the pcs status command.
type PacemakerStatus struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PacemakerStatusSpec   `json:"spec,omitempty"`
	Status PacemakerStatusStatus `json:"status,omitempty"`
}

// PacemakerStatusSpec defines the desired state of PacemakerStatus
type PacemakerStatusSpec struct {
	// NodeName identifies which node this status is for
	// +optional
	NodeName string `json:"nodeName,omitempty"`
}

// PacemakerStatusStatus contains the actual pacemaker cluster status information
type PacemakerStatusStatus struct {
	// LastUpdated is the timestamp when this status was last updated
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// ObservedGeneration is the generation observed by the status collector
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// RawXML contains the raw XML output from pcs status xml command
	// This allows the healthcheck controller to parse it without re-executing the command
	RawXML string `json:"rawXML,omitempty"`

	// CollectionError contains any error encountered while collecting status
	// +optional
	CollectionError string `json:"collectionError,omitempty"`

	// Summary provides a quick overview of the cluster state
	// +optional
	Summary PacemakerSummary `json:"summary,omitempty"`
}

// PacemakerSummary provides a high-level summary of cluster state
type PacemakerSummary struct {
	// PacemakerdState indicates if pacemaker is running
	PacemakerdState string `json:"pacemakerdState,omitempty"`

	// HasQuorum indicates if the cluster has quorum
	HasQuorum bool `json:"hasQuorum,omitempty"`

	// NodesOnline is the count of online nodes
	NodesOnline int `json:"nodesOnline,omitempty"`

	// NodesTotal is the total count of configured nodes
	NodesTotal int `json:"nodesTotal,omitempty"`

	// ResourcesStarted is the count of started resources
	ResourcesStarted int `json:"resourcesStarted,omitempty"`

	// ResourcesTotal is the total count of configured resources
	ResourcesTotal int `json:"resourcesTotal,omitempty"`

	// RecentFailures indicates if there are recent operation failures
	RecentFailures bool `json:"recentFailures,omitempty"`

	// RecentFencing indicates if there are recent fencing events
	RecentFencing bool `json:"recentFencing,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PacemakerStatusList contains a list of PacemakerStatus
type PacemakerStatusList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PacemakerStatus `json:"items"`
}

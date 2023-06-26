package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
//
// # EtcdBackup provides configuration options and status for a one-time backup attempt of the etcd cluster
//
// Compatibility level 4: No compatibility is provided, the API can change at any point for any reason. These capabilities should not be used by applications needing long term support.
// +openshift:compatibility-gen:level=4
type EtcdBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec holds user settable values for configuration
	// +kubebuilder:validation:Required
	// +required
	Spec EtcdBackupSpec `json:"spec"`
	// status holds observed values from the cluster. They may not be overridden.
	// +kubebuilder:validation:Optional
	// +optional
	Status EtcdBackupStatus `json:"status"`
}

type EtcdBackupSpec struct {
	// PVCName specifies the name of the PersistentVolumeClaim which binds a PersistentVolume where the
	// etcd backup file would be saved
	// +kubebuilder:validation:Required
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pvcName is immutable once set"
	PVCName string `json:"pvcName"`
}

const (
	// Etcd backup is running
	Running EtcdBackupState = "Running"
	// Etcd backup is completed
	Completed EtcdBackupState = "Completed"
	// Etcd backup failed
	Failed EtcdBackupState = "Failed"
	// Etcd backup is pending
	Pending EtcdBackupState = "Pending"
)

// etcdBackupState declares valid gathering state types
// +kubebuilder:validation:Optional
// +kubebuilder:validation:Enum=Running;Completed;Failed;Pending
// +kubebuilder:validation:XValidation:rule="!(oldSelf == 'Running' && self == 'Pending')", message="etcdBackupState cannot transition from Running to Pending"
// +kubebuilder:validation:XValidation:rule="!(oldSelf == 'Completed' && self == 'Pending')", message="etcdBackupState cannot transition from Completed to Pending"
// +kubebuilder:validation:XValidation:rule="!(oldSelf == 'Failed' && self == 'Pending')", message="etcdBackupState cannot transition from Failed to Pending"
// +kubebuilder:validation:XValidation:rule="!(oldSelf == 'Completed' && self == 'Running')", message="etcdBackupState cannot transition from Completed to Running"
// +kubebuilder:validation:XValidation:rule="!(oldSelf == 'Failed' && self == 'Running')", message="etcdBackupState cannot transition from Failed to Running"
type EtcdBackupState string

// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.startTime) || has(self.startTime))",message="cannot remove startTime attribute from status"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.finishTime) || has(self.finishTime))",message="cannot remove finishTime attribute from status"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.etcdBackupState) || has(self.etcdBackupState))",message="cannot remove etcdBackupState attribute from status"
// +kubebuilder:validation:Optional
type EtcdBackupStatus struct {
	// conditions provide details on the status of the gatherer job.
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions" patchStrategy:"merge" patchMergeKey:"type"`

	// etcdBackupState reflects the current state of the etcd backup process.
	// +kubebuilder:validation:Optional
	// +optional
	State EtcdBackupState `json:"etcdBackupState,omitempty"`

	// startTime is the time when the etcd backup started.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="startTime is immutable once set"
	// +optional
	StartTime metav1.Time `json:"startTime,omitempty"`

	// finishTime is the time when the etcd backup finished.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="finishTime is immutable once set"
	// +optional
	FinishTime metav1.Time `json:"finishTime,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EtcdBackupList is a collection of items
//
// Compatibility level 4: No compatibility is provided, the API can change at any point for any reason. These capabilities should not be used by applications needing long term support.
// +openshift:compatibility-gen:level=4
type EtcdBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []EtcdBackup `json:"items"`
}

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
//
// Backup provides configuration for performing backups of the openshift cluster.
//
// Compatibility level 4: No compatibility is provided, the API can change at any point for any reason. These capabilities should not be used by applications needing long term support.
// +openshift:compatibility-gen:level=4
type Backup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec holds user settable values for configuration
	// +kubebuilder:validation:Required
	// +required
	Spec BackupSpec `json:"spec"`
	// status holds observed values from the cluster. They may not be overridden.
	// +kubebuilder:validation:Optional
	// +optional
	Status BackupStatus `json:"status"`
}

type BackupSpec struct {
	// +kubebuilder:validation:Required
	EtcdBackupSpec EtcdBackupSpec `json:"etcdBackupSpec"`
}

type BackupStatus struct {
}

// EtcdBackupSpec provides configuration for automated etcd backups to the cluster-etcd-operator
type EtcdBackupSpec struct {

	// Schedule defines the recurring backup schedule in Cron format
	// every 2 hours: 0 */2 * * *
	// every day at 3am: 0 3 * * *
	// Setting to an empty string "" means disabling scheduled backups
	// Default: ""
	// TODO: Define CEL validation for the cron format
	// See if the upstream CronJob has CEL validation already defined somewhere.
	// TODO: Validation to disallow unrealistic schedules (e.g */2 * * * * every 2 mins)
	// +kubebuilder:validation:Optional
	// +optional
	Schedule string `json:"schedule"`

	// The time zone name for the given schedule, see https://en.wikipedia.org/wiki/List_of_tz_database_time_zones.
	// If not specified, this will default to the time zone of the kube-controller-manager process.
	// See https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/#time-zones
	// +kubebuilder:validation:Optional
	// +optional
	// TODO: CEL validation for timezone format
	TimeZone string `json:"timeZone,omitempty"`

	// RetentionPolicy defines the retention policy for retaining and deleting existing backups.
	// +kubebuilder:validation:Optional
	// +optional
	RetentionPolicy RetentionPolicy `json:"retentionPolicy"`

	// PVCName specifies the name of the PersistentVolumeClaim which binds a PersistentVolume where the
	// etcd backup files would be saved
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +required
	PVCName string `json:"pvcName"`
}

// RetentionType is the enumeration of valid retention policy types
// +enum
// +kubebuilder:validation:Enum:="RetentionCount";"RetentionSize"
type RetentionType string

const (
	// RetentionTypeCount sets the retention policy based on the count or number of backup files saved
	RetentionTypeCount RetentionType = "RetentionCount"
	// RetentionTypeSize sets the retention policy based on the total size of the backup files saved
	RetentionTypeSize RetentionType = "RetentionSize"
)

// RetentionPolicy defines the retention policy for retaining and deleting existing backups.
// This struct is a discriminated union that allows users to select the type of retention policy from the supported types.
// +union
type RetentionPolicy struct {
	// RetentionType sets the type of retention policy. The currently supported and valid values are "retentionCount"
	// Currently, the only valid policies are retention by count (RetentionCount) and by size (RetentionSize). More policies or types may be added in the future.
	// +unionDiscriminator
	// +required
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum:=RetentionCount;RetentionSize
	// TODO: How to provide a "NoRetentionPolicy" option as a default? Should that be another enum
	// which would need to be explicitly set as the discriminant or can we set that as the default value.
	RetentionType RetentionType `json:"retentionType"`

	// RetentionCount configures the retention policy based on the number of backups
	// +kubebuilder:validation:Optional
	// +optional
	RetentionCount *RetentionCountConfig `json:"retentionCount,omitempty"`

	// RetentionSize configures the retention policy based on the size of backups
	// +kubebuilder:validation:Optional
	// +optional
	RetentionSize *RetentionSizeConfig `json:"retentionSize,omitempty"`
}

// RetentionCountConfig specifies the configuration of the retention policy on the number of backups
type RetentionCountConfig struct {
	// MaxNumberOfBackups defines the maximum number of backups to retain.
	// If the number of successful backups matches retentionCount
	// the oldest backup will be removed before a new backup is initiated.
	// The count here is for the total number of backups
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Required
	// +required
	MaxNumberOfBackups int `json:"maxNumberOfBackups,omitempty"`
}

// RetentionSizeConfig specifies the configuration of the retention policy on the total size of backups
type RetentionSizeConfig struct {
	// MaxSizeOfBackupsMb defines the total size in Mb of backups to retain.
	// If the current total size backups exceeds MaxSizeOfBackupsMb then
	// the oldest backup will be removed before a new backup is initiated.
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Required
	// +required
	MaxSizeOfBackupsMb int `json:"maxSizeOfBackupsMb,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// BackupList is a collection of items
//
// Compatibility level 4: No compatibility is provided, the API can change at any point for any reason. These capabilities should not be used by applications needing long term support.
// +openshift:compatibility-gen:level=4
type BackupList struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard list's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	metav1.ListMeta `json:"metadata"`
	Items           []Backup `json:"items"`
}

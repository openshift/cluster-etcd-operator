package backuphelpers

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"
)

type Enqueueable interface {
	Enqueue()
}

type BackupVar interface {
	AddListener(listener Enqueueable)
	SetBackupSpec(spec *backupv1alpha1.EtcdBackupSpec)
	ArgString() string
	ProbeString() string
}

type BackupConfig struct {
	enabled   bool
	spec      *backupv1alpha1.EtcdBackupSpec
	listeners []Enqueueable
	mux       sync.Mutex
}

func NewDisabledBackupConfig() *BackupConfig {
	return &BackupConfig{
		enabled: false,
		mux:     sync.Mutex{},
	}
}

func (b *BackupConfig) SetBackupSpec(spec *backupv1alpha1.EtcdBackupSpec) {
	b.mux.Lock()
	defer b.mux.Unlock()

	if reflect.DeepEqual(b.spec, spec) {
		return
	}

	b.enabled = spec != nil

	if spec == nil {
		b.spec = nil
	} else {
		b.spec = spec.DeepCopy()
	}

	for _, l := range b.listeners {
		l.Enqueue()
	}
}

func (b *BackupConfig) ArgString() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	args := []string{"    args:"}
	args = append(args, fmt.Sprintf("- --%s=%v", "enabled", b.enabled))

	if !b.enabled || b.spec == nil {
		return strings.Join(args, "\n    ")
	}

	if b.spec.TimeZone != "" {
		args = append(args, fmt.Sprintf("- --%s=%s", "timezone", b.spec.TimeZone))
	}

	if b.spec.Schedule != "" {
		args = append(args, fmt.Sprintf("- --%s=%s", "schedule", b.spec.Schedule))
	}

	if b.spec.RetentionPolicy.RetentionType == prune.RetentionTypeNumber {
		args = append(args, fmt.Sprintf("- --%s=%s", "type", b.spec.RetentionPolicy.RetentionType))
		args = append(args, fmt.Sprintf("- --%s=%d", "maxNumberOfBackups", b.spec.RetentionPolicy.RetentionNumber.MaxNumberOfBackups))
	} else if b.spec.RetentionPolicy.RetentionType == prune.RetentionTypeSize {
		args = append(args, fmt.Sprintf("- --%s=%s", "type", b.spec.RetentionPolicy.RetentionType))
		args = append(args, fmt.Sprintf("- --%s=%d", "maxSizeOfBackupsGb", b.spec.RetentionPolicy.RetentionSize.MaxSizeOfBackupsGb))
	}

	return strings.Join(args, "\n    ")
}

func (b *BackupConfig) ProbeString() string {
	if !b.enabled {
		return ""
	}

	probeString := []string{"    livenessProbe:", "  httpGet:",
		"    path: healthz", "    port: 8000",
		"    scheme: HTTPS", "  timeoutSeconds: 60",
		"  periodSeconds: 5", "  successThreshold: 1", "  failureThreshold: 5"}

	return strings.Join(probeString, "\n    ")
}

func (b *BackupConfig) AddListener(listener Enqueueable) {
	b.mux.Lock()
	defer b.mux.Unlock()

	b.listeners = append(b.listeners, listener)
}

package backuphelpers

import (
	"fmt"
	"strings"
	"sync"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
)

type BackupVar interface {
	SetBackupSpec(spec backupv1alpha1.EtcdBackupSpec)
	ArgString() string
}

type BackupConfig struct {
	enabled bool
	spec    backupv1alpha1.EtcdBackupSpec
	mux     sync.Mutex
}

func NewDisabledBackupConfig() *BackupConfig {
	return &BackupConfig{
		enabled: false,
		mux:     sync.Mutex{},
	}
}

func (b *BackupConfig) SetBackupSpec(spec backupv1alpha1.EtcdBackupSpec) {
	b.mux.Lock()
	defer b.mux.Unlock()

	b.spec = spec
}

func (b *BackupConfig) ArgString() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	args := []string{"    - backup-server"}
	args = append(args, fmt.Sprintf("- --%s=%v", "enabled", b.enabled))

	if b.spec.TimeZone != "" {
		args = append(args, fmt.Sprintf("- --%s=%s", "timezone", b.spec.TimeZone))
	}

	if b.spec.Schedule != "" {
		args = append(args, fmt.Sprintf("- --%s=%s", "schedule", b.spec.Schedule))
	}

	return strings.Join(args, "\n    ")
}

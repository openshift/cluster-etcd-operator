package backuphelpers

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
)

type Enqueueable interface {
	Enqueue()
}

type BackupVar interface {
	AddListener(listener Enqueueable)
	SetBackupSpec(spec backupv1alpha1.EtcdBackupSpec)
	ArgString() string
}

type BackupConfig struct {
	enabled   bool
	spec      backupv1alpha1.EtcdBackupSpec
	listeners []Enqueueable
	mux       sync.Mutex
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

	if reflect.DeepEqual(b.spec, spec) {
		return
	}

	b.spec = spec
	for _, l := range b.listeners {
		l.Enqueue()
	}
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

func (b *BackupConfig) AddListener(listener Enqueueable) {
	b.mux.Lock()
	defer b.mux.Unlock()

	b.listeners = append(b.listeners, listener)
}

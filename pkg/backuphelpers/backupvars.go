package backuphelpers

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
)

type Enqueueable interface {
	Enqueue()
}

type BackupVar interface {
	AddListener(listener Enqueueable)
	SetBackupSpec(spec *operatorv1alpha1.EtcdBackupPolicySpec)
	ArgString() string
	ArgList() []string
}

type BackupConfig struct {
	enabled   bool
	spec      *operatorv1alpha1.EtcdBackupPolicySpec
	listeners []Enqueueable
	mux       sync.Mutex
}

func NewDisabledBackupConfig() *BackupConfig {
	return &BackupConfig{
		enabled: false,
		mux:     sync.Mutex{},
	}
}

func (b *BackupConfig) SetBackupSpec(spec *operatorv1alpha1.EtcdBackupPolicySpec) {
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

func (b *BackupConfig) ArgList() []string {
	b.mux.Lock()
	defer b.mux.Unlock()
	args := []string{fmt.Sprintf("--%s=%v", "enabled", b.enabled)}

	if !b.enabled || b.spec == nil {
		return args
	}

	if b.spec.TimeZone != "" {
		args = append(args, fmt.Sprintf("--%s=%s", "timezone", b.spec.TimeZone))
	}

	if b.spec.Schedule != "" {
		args = append(args, fmt.Sprintf("--%s=%s", "schedule", b.spec.Schedule))
	}

	return args
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

	return strings.Join(args, "\n    ")
}

func (b *BackupConfig) AddListener(listener Enqueueable) {
	b.mux.Lock()
	defer b.mux.Unlock()

	b.listeners = append(b.listeners, listener)
}

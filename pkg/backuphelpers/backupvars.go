package backuphelpers

import (
	"fmt"
	"strings"
	"sync"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
)

var (
	validBackupVars = map[string]struct{}{
		"schedule":   {},
		"timezone":   {},
		"data-dir":   {},
		"config-dir": {},
		"backup-dir": {},
	}
)

type BackupVar interface {
	GetBackupVars() *BackupConfig
}

type BackupConfig struct {
	enabled  bool
	backupCR backupv1alpha1.EtcdBackupSpec
	mux      sync.Mutex
}

func NewBackupConfig(backupCR backupv1alpha1.EtcdBackupSpec) *BackupConfig {
	return &BackupConfig{
		enabled:  false,
		backupCR: backupCR,
		mux:      sync.Mutex{},
	}
}

func (b *BackupConfig) GetBackupVars() *BackupConfig {
	b.mux.Lock()
	defer b.mux.Unlock()

	c := NewBackupConfig(b.backupCR)
	c.enabled = b.enabled
	return c
}

func (b *BackupConfig) ArgString() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	var args []string
	args = append(args, fmt.Sprintf("- --%s=%s", "timezone", b.backupCR.TimeZone))
	args = append(args, fmt.Sprintf("- --%s=%s", "schedule", b.backupCR.Schedule))

	return strings.Join(args, "\n")
}

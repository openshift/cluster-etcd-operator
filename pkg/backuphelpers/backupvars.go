package backuphelpers

import (
	"fmt"
	"strings"
	"sync"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
)

type BackupVar interface {
	ArgString() string
}

type BackupConfig struct {
	enabled  bool
	backupCR backupv1alpha1.EtcdBackupSpec
	mux      sync.Mutex
}

func NewDisabledBackupConfig(backupCR backupv1alpha1.EtcdBackupSpec, enabled bool) *BackupConfig {
	return &BackupConfig{
		enabled:  enabled,
		backupCR: backupCR,
		mux:      sync.Mutex{},
	}
}

func (b *BackupConfig) ArgString() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	args := []string{"\t- backup-server"}
	if b.enabled {
		args = append(args, fmt.Sprintf("- --%s=%v", "enabled", b.enabled))
	}

	if b.backupCR.TimeZone != "" {
		args = append(args, fmt.Sprintf("- --%s=%s", "timezone", b.backupCR.TimeZone))
	}

	if b.backupCR.Schedule != "" {
		args = append(args, fmt.Sprintf("- --%s=%s", "schedule", b.backupCR.Schedule))
	}

	return strings.Join(args, "\n\t")
}

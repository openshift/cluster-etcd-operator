package backuphelpers

import (
	"fmt"
	"reflect"
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
	ArgString() string
}

type BackupConfig struct {
	enabled  bool
	backupCR backupv1alpha1.EtcdBackupSpec
	mux      sync.Mutex
}

func NewDisabledBackupConfig(backupCR backupv1alpha1.EtcdBackupSpec) *BackupConfig {
	return &BackupConfig{
		enabled:  false,
		backupCR: backupCR,
		mux:      sync.Mutex{},
	}
}

func (b *BackupConfig) ArgString() string {
	b.mux.Lock()
	defer b.mux.Unlock()

	var args []string

	t := reflect.TypeOf(b.backupCR)
	v := reflect.ValueOf(b.backupCR)

	for i := 0; i < t.NumField(); i++ {
		if !v.Field(i).IsZero() {
			argName := strings.ToLower(t.Field(i).Name)
			args = append(args, fmt.Sprintf("\t\t- --%s=%s", argName, v.Field(i).String()))
		}
	}

	return strings.Join(args, "\n")
}

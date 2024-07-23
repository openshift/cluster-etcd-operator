package backuphelpers

import "sync"

type BackupVar interface {
	GetBackupVars() map[string]string
}

type BackupConfig struct {
	schedule  string
	timeZone  string
	dataDir   string
	configDir string
	backupDir string
	mux       sync.Mutex
}

func NewBackupConfig(schedule, timeZone, dataDir, configDir, backupDir string) *BackupConfig {
	return &BackupConfig{
		schedule:  schedule,
		timeZone:  timeZone,
		dataDir:   dataDir,
		configDir: configDir,
		backupDir: backupDir,
		mux:       sync.Mutex{},
	}
}

func (b *BackupConfig) GetBackupVars() map[string]string {
	b.mux.Lock()
	defer b.mux.Unlock()

	vars := map[string]string{}
	vars["schedule"] = b.schedule
	vars["timezone"] = b.timeZone
	vars["data-dir"] = b.dataDir
	vars["config-dir"] = b.configDir
	vars["backup-dir"] = b.backupDir

	return vars
}

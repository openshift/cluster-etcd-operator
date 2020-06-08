package rollbackcopy

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
)

type dirEntry struct {
	name      string
	version   string
	timestamp string
}

func createBackupDir(basedirpath string, dir *dirEntry) error {
	fullpath := filepath.Join(basedirpath, dir.name)
	if err := checkAndCreateDir(fullpath); err != nil {
		return fmt.Errorf("createBackupDir failed: %w", err)
	}
	backupVersion := &backupVersion{ClusterVersion: dir.version, TimeStamp: dir.timestamp}
	if err := putVersion(backupVersion, fullpath); err != nil {
		return fmt.Errorf("createBackupDir: putVersion failed: %w", err)
	}
	return nil
}

func checkBackupDir(basedirpath string, dir *dirEntry) error {
	fullpath := filepath.Join(basedirpath, dir.name)
	isDir, err := dirExists(fullpath)
	if err != nil {
		return fmt.Errorf("checkBackupDir: dirExists on %s failed: %w", fullpath, err)
	}

	if !isDir {
		return fmt.Errorf("checkBackupDir: %s is not a directory", fullpath)
	}

	backupVersion, err := getVersion(fullpath)
	if err != nil {
		return fmt.Errorf("checkBackupDir: getVersion failed: %w", err)
	}
	if backupVersion.ClusterVersion != dir.version || backupVersion.TimeStamp != dir.timestamp {
		return fmt.Errorf("Directory mismatch: expected %v, but got %v", dir, backupVersion)
	}
	return nil
}

func Test_checkVersionsAndMoveDirs(t *testing.T) {
	type args struct {
		newBackupDir     dirEntry
		updateInProgress bool
	}
	configDir, err := ioutil.TempDir(os.TempDir(), "kubernetes-")
	if err != nil {
		t.Fatalf("Fatal: %v", err)
	}
	t.Logf("ConfigDir: %s", configDir)
	defer os.RemoveAll(configDir) // clean up

	tests := []struct {
		name         string
		args         args
		existingDirs []dirEntry
		want         []dirEntry
		wantErr      bool
	}{
		{
			name: "Successful initial backup",
			args: args{
				newBackupDir:     dirEntry{"tmp", "4.4.10-0", "2020-06-12_220858"},
				updateInProgress: false,
			},
			existingDirs: []dirEntry{},
			want: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
			},
		},
		{
			name: "Successful periodic backup after the initial backup",
			args: args{
				newBackupDir:     dirEntry{"tmp", "4.4.10-0", "2020-06-12_230858"},
				updateInProgress: false,
			},
			existingDirs: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
			},
			want: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_230858"},
				{"currentVersion.prev", "4.4.10-0", "2020-06-12_220858"},
			},
		},
		{
			name: "Don't try to move directories during upgrade",
			args: args{
				newBackupDir:     dirEntry{"tmp", "4.4.11-0", "2020-06-12_230858"},
				updateInProgress: true,
			},
			existingDirs: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"olderVersion.latest", "4.4.9-0", "2020-06-10_010858"},
			},
			want: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"olderVersion.latest", "4.4.9-0", "2020-06-10_010858"},
			},
		},
		{
			name: "backup after rollout of new Y at time T",
			args: args{
				newBackupDir:     dirEntry{"tmp", "4.4.11-0", "2020-06-12_230858"},
				updateInProgress: false,
			},
			existingDirs: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"currentVersion.prev", "4.4.10-0", "2020-06-12_210858"},
			},
			want: []dirEntry{
				{"currentVersion.latest", "4.4.11-0", "2020-06-12_230858"},
				{"olderVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"olderVersion.prev", "4.4.10-0", "2020-06-12_210858"},
			},
		},
		{
			name: "successful periodic backup at time T preserves all prior backups",
			args: args{
				newBackupDir:     dirEntry{"tmp", "4.4.10-0", "2020-06-12_230858"},
				updateInProgress: false,
			},
			existingDirs: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"currentVersion.prev", "4.4.10-0", "2020-06-12_210858"},
				{"olderVersion.latest", "4.4.9-0", "2020-06-12_200858"},
				{"olderVersion.prev", "4.4.9-0", "2020-06-12_190858"},
			},
			want: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_230858"},
				{"currentVersion.prev", "4.4.10-0", "2020-06-12_220858"},
				{"olderVersion.latest", "4.4.9-0", "2020-06-12_200858"},
				{"olderVersion.prev", "4.4.9-0", "2020-06-12_190858"},
			},
		},
		{
			name: "Upgrade after time T",
			args: args{
				newBackupDir:     dirEntry{"tmp", "4.4.11-0", "2020-06-12_230858"},
				updateInProgress: false,
			},
			existingDirs: []dirEntry{
				{"currentVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"currentVersion.prev", "4.4.10-0", "2020-06-12_210858"},
				{"olderVersion.latest", "4.4.9-0", "2020-06-12_200858"},
				{"olderVersion.prev", "4.4.9-0", "2020-06-12_190858"},
			},
			want: []dirEntry{
				{"currentVersion.latest", "4.4.11-0", "2020-06-12_230858"},
				{"olderVersion.latest", "4.4.10-0", "2020-06-12_220858"},
				{"olderVersion.prev", "4.4.10-0", "2020-06-12_210858"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDirPath := filepath.Join(configDir, "rollbackcopy")
			os.RemoveAll(baseDirPath)
			if err := createBackupDir(baseDirPath, &tt.args.newBackupDir); err != nil {
				t.Fatalf("createDirWithVersion() baseDir=%s, %v, error= %v", baseDirPath, tt.args.newBackupDir, err)
			}
			for _, existingDir := range tt.existingDirs {
				if err := createBackupDir(baseDirPath, &existingDir); err != nil {
					t.Errorf("createDirWithVersion() error= %v", err)
				}
			}

			if err := checkVersionsAndMoveDirs(configDir, filepath.Join(baseDirPath, tt.args.newBackupDir.name), tt.args.updateInProgress); (err != nil) != tt.wantErr {
				t.Errorf("checkVersionsAndMoveDirs() error = %v, wantErr %v", err, tt.wantErr)
			}
			for _, wantDir := range tt.want {
				if err := checkBackupDir(baseDirPath, &wantDir); err != nil {
					t.Errorf("checkBackupDir() error= %v", err)
				}
			}
		})
	}
}

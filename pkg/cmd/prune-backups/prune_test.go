package prune_backups

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCommandValidation(t *testing.T) {
	testCases := map[string]struct {
		opts        PruneOpts
		expectedErr error
	}{
		"none happy":   {opts: PruneOpts{RetentionType: RetentionTypeNone}},
		"number happy": {opts: PruneOpts{RetentionType: RetentionTypeNumber, MaxNumberOfBackups: 1}},
		"number zero": {opts: PruneOpts{RetentionType: RetentionTypeNumber, MaxNumberOfBackups: 0},
			expectedErr: fmt.Errorf("unexpected amount of backups [0] found, expected at least 1")},
		"number flipped": {opts: PruneOpts{RetentionType: RetentionTypeNumber, MaxNumberOfBackups: 2, MaxSizeOfBackupsGb: 25},
			expectedErr: fmt.Errorf("unexpected argument [MaxSizeOfBackupsGb] found while using RetentionNumber")},

		"size happy": {opts: PruneOpts{RetentionType: RetentionTypeSize, MaxSizeOfBackupsGb: 1}},
		"size zero": {opts: PruneOpts{RetentionType: RetentionTypeSize, MaxSizeOfBackupsGb: 0},
			expectedErr: fmt.Errorf("unexpected size of backups [0]gb found, expected at least 1")},
		"size flipped": {opts: PruneOpts{RetentionType: RetentionTypeSize, MaxSizeOfBackupsGb: 2, MaxNumberOfBackups: 25},
			expectedErr: fmt.Errorf("unexpected argument [MaxNumberOfBackups] found while using RetentionSize")},
	}

	for k, v := range testCases {
		t.Run(k, func(t *testing.T) {
			err := (&v.opts).Validate()
			require.Equal(t, v.expectedErr, err)
		})
	}
}

func TestRetainByNumber(t *testing.T) {
	temp, err := os.MkdirTemp("", "TestRetainByNumber")
	t.Cleanup(func() {
		_ = os.RemoveAll(temp)
	})
	require.NoError(t, err)
	expectedFolders := []string{"backup-1", "backup-2", "backup-3", "backup-4", "backup-5"}
	for _, folder := range expectedFolders {
		makefile(t, temp, folder, randBytes(5))
		// the creation is fairly quick, so the mod time doesn't reflect as it otherwise would
		time.Sleep(1 * time.Second)
	}

	require.NoError(t, retainByNumber(6, temp))
	requireFoldersExist(t, temp, expectedFolders)
	require.NoError(t, retainByNumber(5, temp))
	requireFoldersExist(t, temp, expectedFolders)
	require.NoError(t, retainByNumber(3, temp))
	requireFoldersExist(t, temp, []string{"backup-3", "backup-4", "backup-5"})
	require.NoError(t, retainByNumber(1, temp))
	requireFoldersExist(t, temp, []string{"backup-5"})
	require.NoError(t, retainByNumber(0, temp))
	requireFoldersExist(t, temp, []string{})
}

// that test can use some fairly large amount of your disk space ;-)
func TestRetainBySize(t *testing.T) {
	temp, err := os.MkdirTemp("", "TestRetainBySize")
	t.Cleanup(func() {
		_ = os.RemoveAll(temp)
	})
	require.NoError(t, err)
	expectedFolders := []string{"backup-1", "backup-2", "backup-3", "backup-4", "backup-5"}
	// using a zeroed slice here, being much faster than generating a random gigabyte of bytes
	zeroGig := make([]byte, 1024*1024*1024*1)
	for _, folder := range expectedFolders {
		makefile(t, temp, folder, zeroGig)
	}

	requireFoldersExist(t, temp, expectedFolders)
	require.NoError(t, retainBySizeGb(10, temp))

	for i := 4; i >= 0; i-- {
		require.NoError(t, retainBySizeGb(i, temp))
		requireFoldersExist(t, temp, expectedFolders[len(expectedFolders)-i:])
	}

	// in the end we should arrive at the empty list, which would be a no-op when retaining with 0.
	requireFoldersExist(t, temp, []string{})
	require.NoError(t, retainBySizeGb(0, temp))
	requireFoldersExist(t, temp, []string{})
}

func TestDeleteByName(t *testing.T) {
	t.Run("dirs", func(t *testing.T) {
		temp, err := os.MkdirTemp("", "TestDeleteByName")
		t.Cleanup(func() {
			_ = os.RemoveAll(temp)
		})
		require.NoError(t, err)
		expectedFolders := []string{"backup-1", "backup-2", "backup-3", "backup-4", "backup-5"}
		for _, folder := range expectedFolders {
			makefile(t, temp, folder, randBytes(5))
		}
		requireFoldersExist(t, temp, expectedFolders)

		err = deleteByName(expectedFolders[:2], temp)
		require.NoError(t, err)
		requireFoldersExist(t, temp, expectedFolders[2:])

		err = deleteByName(expectedFolders, temp)
		require.NoError(t, err)
		requireFoldersExist(t, temp, []string{})
	})
	t.Run("files", func(t *testing.T) {
		temp, err := os.MkdirTemp("", "TestDeleteByName")
		t.Cleanup(func() {
			_ = os.RemoveAll(temp)
		})
		require.NoError(t, err)
		expectedFolders := []string{"backup-1", "backup-2", "backup-3", "backup-4", "backup-5"}
		expectedFiles := make([]string, len(expectedFolders))
		for i, folder := range expectedFolders {
			_, fileName := makefile(t, temp, folder, randBytes(5))
			expectedFiles[i] = fileName
		}
		requireFiles(t, expectedFiles, true)

		err = deleteByName(expectedFiles[:2], temp)
		require.NoError(t, err)
		requireFiles(t, expectedFiles[2:], true)
		requireFiles(t, expectedFiles[:2], false)
		requireFoldersExist(t, temp, expectedFolders[2:])

		err = deleteByName(expectedFiles, temp)
		require.NoError(t, err)
		requireFoldersExist(t, temp, []string{})
	})
}

func TestValidateFileNames(t *testing.T) {
	datetime := time.Now().Format("2006-01-02_150405")
	testCases := []struct {
		name      string
		fileNames []string
		isValid   bool
	}{
		{
			name: "valid-files",
			fileNames: []string{
				"/etc/kubernetes/cluster-backup/snapshot_" + datetime + ".db",
				"/etc/kubernetes/cluster-backup/snapshot_" + datetime + ".db.part",
				"/etc/kubernetes/cluster-backup/static_kuberesources_" + datetime + ".tar.gz",
			},
			isValid: true,
		},
		{
			name: "valid-files-on-custom-path",
			fileNames: []string{
				"/some/random/dir/snapshot_" + datetime + ".db",
				"/some/random/dir/static_kuberesources_" + datetime + ".tar.gz",
			},
			isValid: true,
		},
		{
			name: "some-valid-one-invalid-file",
			fileNames: []string{
				"/etc/kubernetes/cluster-backup/snapshot_" + datetime + ".db",
				"/etc/kubernetes/cluster-backup/snapshot_" + datetime + ".db.part",
				"/etc/kubernetes/cluster-backup/static_kuberesources_" + datetime + ".db",
			},
			isValid: false,
		},
		{
			name: "invalid-file",
			fileNames: []string{
				"/etc/secrets/dont-delete-me",
			},
			isValid: false,
		},
		{
			name: "invalid-file-prefix",
			fileNames: []string{
				"/etc/kubernetes/cluster-backup/different_snapshot_" + datetime + ".db",
			},
			isValid: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			temp, err := os.MkdirTemp("", "TestValidateFileNames")
			t.Cleanup(func() {
				_ = os.RemoveAll(temp)
			})
			require.NoError(t, err)
			filePaths := make([]string, 0, len(tc.fileNames))
			for _, fileName := range tc.fileNames {
				fileName = path.Join(temp, fileName)
				require.NoError(t, os.MkdirAll(path.Dir(fileName), 0750))
				require.NoError(t, os.WriteFile(fileName, randBytes(5), 0600))
				filePaths = append(filePaths, fileName)
			}
			requireFiles(t, filePaths, true)

			err = deleteByName(filePaths, temp)
			if tc.isValid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestBackupFolderLister(t *testing.T) {
	temp, err := os.MkdirTemp("", "TestBackupFolderLister")
	t.Cleanup(func() {
		_ = os.RemoveAll(temp)
	})
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(path.Join(temp, "backup-1"), 0750))
	require.NoError(t, os.WriteFile(path.Join(temp, "backup-1", "snapshot.snap"), randBytes(2500), 0600))

	require.NoError(t, os.MkdirAll(path.Join(temp, "backup-2"), 0750))
	require.NoError(t, os.WriteFile(path.Join(temp, "backup-2", "snapshot.snap"), randBytes(5500), 0600))
	require.NoError(t, os.MkdirAll(path.Join(temp, "backup-2", "random-other-folder"), 0750))
	require.NoError(t, os.WriteFile(path.Join(temp, "backup-2", "random-other-folder", "some.yaml"), randBytes(2222), 0600))

	require.NoError(t, os.MkdirAll(path.Join(temp, "backup-3"), 0750))
	require.NoError(t, os.WriteFile(path.Join(temp, "backup-3", "snapshot.snap"), randBytes(51500), 0600))

	folders, err := listAllBackupFolders(temp)
	require.NoError(t, err)

	// we rely on the alphabetical ordering of the fs.WalkDir methods here
	require.Equal(t, 3, len(folders))
	require.Equal(t, "backup-1", folders[0].name)
	require.Equal(t, int64(2500), folders[0].sizeBytes)

	require.Equal(t, "backup-2", folders[1].name)
	require.Equal(t, int64(5500+2222), folders[1].sizeBytes)

	require.Equal(t, "backup-3", folders[2].name)
	require.Equal(t, int64(51500), folders[2].sizeBytes)
}

func TestDirectorySorting(t *testing.T) {
	var stats backupDirStats
	stats = append(stats, backupDirStat{name: "c", modTime: time.Unix(5500, 0)})
	stats = append(stats, backupDirStat{name: "a", modTime: time.Unix(2500, 0)})
	stats = append(stats, backupDirStat{name: "b", modTime: time.Unix(3500, 0)})

	sort.Sort(stats)
	var names []string
	for _, a := range stats {
		names = append(names, a.name)
	}
	require.Equal(t, []string{"c", "b", "a"}, names)
}

func TestDirectorySortingByName(t *testing.T) {
	var stats backupDirStats
	stats = append(stats, backupDirStat{name: "backup-name-2026-01-02_150405"})
	stats = append(stats, backupDirStat{name: "backup-name-2022-01-02_150405"})
	stats = append(stats, backupDirStat{name: "backup-name-2022-01-02_50405"})

	sort.Sort(stats)
	var names []string
	for _, a := range stats {
		names = append(names, a.name)
	}
	require.Equal(t, []string{
		"backup-name-2026-01-02_150405",
		"backup-name-2022-01-02_50405",
		"backup-name-2022-01-02_150405",
	}, names)
}

func randBytes(n int) []byte {
	var s []byte
	for range n {
		s = append(s, byte(rand.Intn(255)))
	}
	return s
}

func requireFoldersExist(t *testing.T, basePath string, expectedFolders []string) {
	dir, err := os.ReadDir(basePath)
	require.NoError(t, err)
	require.Equal(t, len(expectedFolders), len(dir), "unexpected amount of directories found")
	var actualFolders []string
	for _, d := range dir {
		actualFolders = append(actualFolders, d.Name())
	}
	require.ElementsMatchf(t, actualFolders, expectedFolders, "unexpected folders found")

}

func requireFiles(t *testing.T, files []string, exist bool) {
	t.Helper()
	var actualFiles []string
	for _, fileName := range files {
		if _, err := os.Stat(fileName); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				require.NoError(t, err)
			}
		} else {
			actualFiles = append(actualFiles, fileName)
		}
	}

	if exist {
		require.ElementsMatchf(t, actualFiles, files, "expected files to exist")
	} else {
		require.Len(t, actualFiles, 0, "expected files to not exist")
	}
}

func makefile(t *testing.T, temp, dir string, data []byte) (dirName, fileName string) {
	t.Helper()
	dirName = path.Join(temp, dir)
	fileName = path.Join(temp, dir, "snapshot.snap")
	require.NoError(t, os.MkdirAll(dirName, 0750))
	require.NoError(t, os.WriteFile(fileName, data, 0600))
	return dirName, fileName
}

func preventDeletion(t *testing.T, dirName, fileName string) (deferred func() error) {
	t.Helper()
	// Prevent deletion of file in backup-1 dir
	if runtime.GOOS == "windows" {
		file, err := os.Open(fileName)
		require.NoError(t, err)
		return file.Close
	}
	os.Chmod(dirName, 0550)
	return func() error {
		return os.Chmod(dirName, 0750)
	}
}

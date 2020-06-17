package rollbackcopy

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"k8s.io/klog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func dirExists(dirname string) (bool, error) {
	info, err := os.Stat(dirname)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("dirExists: %s. Error=%w", dirname, err)
	}
	return info.IsDir(), nil
}

func safeDirRename(src, dest string, srcMayNotExist bool) error {
	srcPresent, err := dirExists(src)

	if err != nil {
		return fmt.Errorf("safeDirRename: Unexpected error checking dir: %s. Error: %w", src, err)
	}

	if !srcPresent {
		if !srcMayNotExist {
			return fmt.Errorf("SafeDirRename: src dir %s does not exist", src)
		} else {
			return nil
		}
	}

	destExists, err := dirExists(dest)
	if err != nil {
		return fmt.Errorf("safeDirRename: Unexpected error checking dir: %s. Error: %w", dest, err)
	}

	destToBeRemoved := dest + ".to_be_removed"
	if destExists {
		if exists, _ := dirExists(destToBeRemoved); exists {
			if err := os.RemoveAll(destToBeRemoved); err != nil {
				return fmt.Errorf("safeDirRename: failed to remove dir %s. Error: %w", destToBeRemoved, err)
			}
		}
		if err := os.Rename(dest, destToBeRemoved); err != nil {
			return fmt.Errorf("safeDirRename: got error renaming %s %s. Err: %w", dest, destToBeRemoved, err)
		}
	}

	if err := os.Rename(src, dest); err != nil {
		if destExists {
			if err := os.Rename(destToBeRemoved, dest); err != nil {
				klog.Errorf("safeDirRename: got error renaming %s to %s. Error: %v", destToBeRemoved, dest, err)
			}
		}
		return fmt.Errorf("safeDirRename: got error renaming %s to %s. Err: %w", src, dest, err)
	}

	klog.Info("Successfully moved ", src, " to ", dest)

	if err := os.RemoveAll(destToBeRemoved); err != nil {
		klog.Errorf("safeDirRename: got error removing %s. Error: %v", destToBeRemoved, err)
	}
	return nil
}

func dumpDirs(dir string) error {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("DumpDirs: could not read dir %s: %w", dir, err)
	}

	klog.Infof("dumpDirs: Dumping currently saved directories in %s:", dir)
	for _, f := range files {
		if f.IsDir() {
			dirname := filepath.Join(dir, f.Name())
			if dirVersion, err := getVersion(dirname); err != nil {
				klog.Errorf("dumpDirs: Could not dump dir %s. Error: %w", dirname, err)
			} else {
				klog.Infof("\t%s ClusterVersion: %s TimeStamp: %s", dirname, dirVersion.ClusterVersion, dirVersion.TimeStamp)
			}
		}
	}
	return nil
}

func findTheLatestRevision(dir, podname string) (string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var modTime time.Time
	var latest string
	found := false
	for _, f := range files {
		if f.IsDir() && strings.HasPrefix(f.Name(), podname) {
			if f.ModTime().After(modTime) {
				modTime = f.ModTime()
				latest = f.Name()
				found = true
			}
		}
	}
	if !found {
		return "", fmt.Errorf("Not found any resources for pod %s", podname)
	}
	return filepath.Join(dir, latest), nil
}

type backupVersion struct {
	ClusterVersion string `json:"ClusterVersion"`
	TimeStamp      string `json:"TimeStamp"`
}

func versionChanged(dir1, dir2 string) bool {
	dir1Version, err := getVersion(dir1)
	if err != nil {
		klog.Errorf("getVersion failed on %s", dir1)
		return false
	}
	dir2Version, err := getVersion(dir2)
	if err != nil {
		klog.Errorf("getVersion failed on %s", dir2)
		return false
	}
	klog.Infof("versionCheck: Dir: %s Version: %s", dir1, dir1Version.ClusterVersion)
	klog.Infof("versionCheck: Dir: %s Version: %s", dir2, dir2Version.ClusterVersion)
	if dir1Version.ClusterVersion != dir2Version.ClusterVersion {
		klog.Infof("Version changed from %s to %s", dir1Version.ClusterVersion, dir2Version.ClusterVersion)
	}
	return (dir1Version.ClusterVersion != dir2Version.ClusterVersion)
}

func getVersion(dir string) (*backupVersion, error) {
	version := backupVersion{}
	jsonFile, err := ioutil.ReadFile(filepath.Join(dir, "backupenv.json"))
	if err != nil {
		return nil, fmt.Errorf("getVersion failed: %w", err)
	}
	if err := json.Unmarshal(jsonFile, &version); err != nil {
		return nil, fmt.Errorf("getVersion: json unmarshal error: %w", err)
	}

	return &version, nil
}

func putVersion(c *backupVersion, dir string) error {
	confBytes, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("putVersion json marshal error: %w", err)
	}

	if err := ioutil.WriteFile(filepath.Join(dir, "backupenv.json"), confBytes, 0644); err != nil {
		return fmt.Errorf("putVersion: writeFile failed: %w", err)
	}

	return nil
}

func checkAndCreateDir(dirName string) error {
	_, err := os.Stat(dirName)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checkAndCreateDir failed: %w", err)
	}
	// If dirName already exists, remove it
	if err == nil {
		if err := os.RemoveAll(dirName); err != nil {
			return fmt.Errorf("checkAndCreateDir failed to remove %s: %w", dirName, err)
		}
	}
	if err := os.MkdirAll(dirName, os.ModePerm); err != nil {
		return fmt.Errorf("checkAndCreateDir failed: %w", err)
	}
	return nil
}

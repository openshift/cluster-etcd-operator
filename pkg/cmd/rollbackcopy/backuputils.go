package rollbackcopy

import (
	"fmt"
	"k8s.io/klog"
	"os"
	"path/filepath"
	"time"
)

//This backup mimics the functionality of cluster-backup.sh

var backupResourcePodList = []string{
	"kube-apiserver-pod",
	"kube-controller-manager-pod",
	"kube-scheduler-pod",
	"etcd-pod",
}

func archiveLatestResources(configDir, backupFile string) error {
	klog.Info("Static Pod Resources are being stored in: ", backupFile)

	paths := []string{}
	for _, podName := range backupResourcePodList {
		latestPod, err := findTheLatestRevision(filepath.Join(configDir, "static-pod-resources"), podName)
		if err != nil {
			return fmt.Errorf("findTheLatestRevision failed: %w", err)
		}
		paths = append(paths, latestPod)
		klog.Info("\tAdding the latest revision for podName ", podName, ": ", latestPod)
	}

	err := createTarball(backupFile, paths, configDir)
	if err != nil {
		return fmt.Errorf("Got error creating the tar archive: %w", err)
	}
	return nil
}

func backup(configDir string) error {
	cli, err := getEtcdClient([]string{"localhost:2379"})
	if err != nil {
		return fmt.Errorf("backup: failed to get etcd client: %w", err)
	}
	defer cli.Close()

	currentClusterVersion, upgradeInProgress, err := getClusterVersionAndUpgradeInfo(cli)
	if err != nil {
		return fmt.Errorf("getClusterVersionAndUpgradeInfo failed: %w", err)
	}

	if upgradeInProgress {
		return fmt.Errorf("The cluster is being upgraded. Skipping backup!")
	}

	tmpBackupDir := filepath.Join(configDir, "rollbackcopy", "tmp")
	defer os.RemoveAll(tmpBackupDir)

	if err := checkAndCreateDir(tmpBackupDir); err != nil {
		return fmt.Errorf("backup: checkAndCreateDir failed: %w", err)
	}

	// Trying to match the output file formats with the formats of the current cluster-backup.sh script
	dateString := time.Now().Format("2006-01-02_150405")
	outputArchive := "static_kuberesources_" + dateString + ".tar.gz"
	snapshotOutFile := "snapshot_" + dateString + ".db"

	// Save snapshot
	if err := saveSnapshot(cli, filepath.Join(tmpBackupDir, snapshotOutFile)); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}

	// Save the corresponding static pod resources
	if err := archiveLatestResources(configDir, filepath.Join(tmpBackupDir, outputArchive)); err != nil {
		return fmt.Errorf("archiveLatestResources failed: %w", err)
	}

	// Write the version
	version := backupVersion{currentClusterVersion, dateString}
	if err := putVersion(&version, tmpBackupDir); err != nil {
		return fmt.Errorf("putVersion failed: %w", err)
	}

	if err := checkVersionsAndMoveDirs(configDir, tmpBackupDir, upgradeInProgress); err != nil {
		return fmt.Errorf("checkVersionsAndMoveDirs failed: %w", err)
	}

	dumpDirs(filepath.Join(configDir, "rollbackcopy"))

	return nil
}

func checkVersionsAndMoveDirs(configDir, newBackupDir string, beingUpgraded bool) error {
	if beingUpgraded {
		return nil
	}
	currentVersionlatestDir := filepath.Join(configDir, "rollbackcopy", "currentVersion.latest")
	currentVersionPrevDir := filepath.Join(configDir, "rollbackcopy", "currentVersion.prev")
	if versionChanged(currentVersionlatestDir, newBackupDir) {
		olderVersionlatestDir := filepath.Join(configDir, "rollbackcopy", "olderVersion.latest")
		olderVersionPrevDir := filepath.Join(configDir, "rollbackcopy", "olderVersion.prev")
		if err := safeDirRename(currentVersionPrevDir, olderVersionPrevDir, true); err != nil {
			return err
		}
		if err := safeDirRename(currentVersionlatestDir, olderVersionlatestDir, true); err != nil {
			return err
		}
	} else {
		if err := safeDirRename(currentVersionlatestDir, currentVersionPrevDir, true); err != nil {
			return err
		}
	}
	if err := safeDirRename(newBackupDir, currentVersionlatestDir, false); err != nil {
		return err
	}
	klog.Info("Backed up resources and snapshot to ", currentVersionlatestDir)
	return nil
}

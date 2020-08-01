package backuprestore

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var static_pod_list = []string{
	"etcd-pod.yaml",
	"kube-apiserver-pod.yaml",
	"kube-controller-manager-pod.yaml",
	"kube-scheduler-pod.yaml",
}

func restore(configDir, dataDir, backupDir string) error {

	var (
		assetDir           = filepath.Join(configDir, "assets")
		manifestDir        = filepath.Join(configDir, "manifests")
		manifestStoppedDir = filepath.Join(assetDir, "manifests-stopped")
		restoreEtcdPodYaml = filepath.Join(configDir, "static-pod-resources", "etcd-certs", "configmaps", "restore-etcd-pod", "pod.yaml")
		dataDirBackup      = dataDir + "-backup"
	)
	fmt.Printf("configDir=%s,dataDir=%s,backupDir=%s\n", configDir, dataDir, backupDir)
	// locate snapshot db and static pod resources archive
	snapshotFile, err := findTheLatestRevision(backupDir, "snapshot_", false)
	if err != nil {
		return fmt.Errorf("restore: could not find snapshot db: %w", err)
	}
	resourcesArchive, err := findTheLatestRevision(backupDir, "static_kuberesources_", false)
	if err != nil {
		return fmt.Errorf("restore: could not find static pod resources: %w", err)
	}

	// Move manifests to manifests-stopped-dir
	if err := checkAndCreateDir(manifestStoppedDir); err != nil {
		return fmt.Errorf("restore: checkAndCreateDir failed: %w", err)
	}
	for _, podyaml := range static_pod_list {
		err := os.Rename(filepath.Join(manifestDir, podyaml), filepath.Join(manifestStoppedDir, podyaml))
		if err != nil {
			return fmt.Errorf("restore: attempt to stop %s failed: %w", podyaml, err)
		}
	}

	// TODO: Wait for static pods to stop
	// crictl ps equivalent??
	time.Sleep(180 * time.Second)

	dataDirMember := filepath.Join(dataDir, "member")
	dataDirMemberExists, err := dirExists(dataDirMember)
	if err != nil {
		return fmt.Errorf("restore: Unexpected error checking dir: %s. Error: %w", dataDirMember, err)
	}
	if dataDirMemberExists {
		// backup data-dir to data-dir-backup
		if err := checkAndCreateDir(dataDirBackup); err != nil {
			return fmt.Errorf("restore: checkAndCreateDir failed: %w", err)
		}
		// check if data-dir-backup already exists
		dataDirBackupMember := filepath.Join(dataDirBackup, "member")
		backupMemberExists, err := dirExists(dataDirBackupMember)
		if err != nil {
			return fmt.Errorf("restore: Unexpected error checking dir: %s. Error: %w", dataDirBackupMember, err)
		}
		// if old backup exists, remove all
		if backupMemberExists {
			os.RemoveAll(dataDirBackupMember)
		}
		// Rename data-dir/member to data-dir-backup/member
		if err := os.Rename(dataDirMember, dataDirBackupMember); err != nil {
			return fmt.Errorf("restore: attempt to backup data-dir %s failed: %w", dataDir, err)
		}
	}

	// Restore static pod resources
	if err := extractFromTarGz(resourcesArchive, configDir); err != nil {
		return fmt.Errorf("restore: attempt to extract static-pod-resources from archive %s failed: %w",
			resourcesArchive, err)
	}

	// Copy snapshot to backupdir
	etcdDataDirBackupSnapshot := dataDirBackup + "snapshot.db"
	if _, err := fileCopy(snapshotFile, etcdDataDirBackupSnapshot); err != nil {
		return fmt.Errorf("restore: attempt to copy snapshot %s failed: %w", snapshotFile, err)
	}

	// Copy restore etcd pod to manifest directory
	if _, err := fileCopy(restoreEtcdPodYaml, filepath.Join(manifestDir, "etcd-pod.yaml")); err != nil {
		return fmt.Errorf("restore: attempt to copy restore etcd %s failed: %w", restoreEtcdPodYaml, err)
	}

	// Restore remaining static pods
	for _, podyaml := range static_pod_list {
		if podyaml == "etcd-pod.yaml" {
			continue
		}
		if err := extractFileFromTarGz(resourcesArchive, manifestDir, podyaml); err != nil {
			return fmt.Errorf("restore: attempt to extract manifest file from archive %s for pod %s failed: %w",
				resourcesArchive, podyaml, err)
		}

	}
	return nil

}

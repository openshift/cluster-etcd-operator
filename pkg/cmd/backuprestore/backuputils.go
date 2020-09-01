package backuprestore

import (
	"fmt"
	"k8s.io/klog/v2"
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
		latestPod, err := findTheLatestRevision(filepath.Join(configDir, "static-pod-resources"), podName, true)
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

func backup(r *backupOptions) error {
	cli, err := getEtcdClient(r.endpoints)
	if err != nil {
		return fmt.Errorf("backup: failed to get etcd client: %w", err)
	}
	defer cli.Close()

	if err := checkAndCreateDir(r.backupDir); err != nil {
		return fmt.Errorf("backup: checkAndCreateDir failed: %w", err)
	}

	// Trying to match the output file formats with the formats of the current cluster-backup.sh script
	dateString := time.Now().Format("2006-01-02_150405")
	outputArchive := "static_kuberesources_" + dateString + ".tar.gz"
	snapshotOutFile := "snapshot_" + dateString + ".db"

	// Save snapshot
	if err := saveSnapshot(cli, filepath.Join(r.backupDir, snapshotOutFile)); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}

	// Save the corresponding static pod resources
	if err := archiveLatestResources(r.configDir, filepath.Join(r.backupDir, outputArchive)); err != nil {
		return fmt.Errorf("archiveLatestResources failed: %w", err)
	}

	return nil
}

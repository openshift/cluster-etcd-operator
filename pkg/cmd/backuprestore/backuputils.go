package backuprestore

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"k8s.io/klog/v2"
)

//This backup mimics the functionality of cluster-backup.sh

var backupResourcePodList = []string{
	"kube-apiserver-pod",
	"kube-controller-manager-pod",
	"kube-scheduler-pod",
	"etcd-pod",
}

func archiveLatestResources(configDir, backupFile string) (int64, error) {
	klog.Info("Static Pod Resources are being stored in: ", backupFile)

	paths := []string{}
	for _, podName := range backupResourcePodList {
		latestPod, err := findTheLatestRevision(filepath.Join(configDir, "static-pod-resources"), podName, true)
		if err != nil {
			return 0, fmt.Errorf("findTheLatestRevision failed: %w", err)
		}
		paths = append(paths, latestPod)
		klog.Info("\tAdding the latest revision for podName ", podName, ": ", latestPod)
	}

	size, err := createTarball(backupFile, paths, configDir)
	if err != nil {
		return 0, fmt.Errorf("Got error creating the tar archive: %w", err)
	}
	return size, nil
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
	snapshotFilepath := filepath.Join(r.backupDir, snapshotOutFile)
	archiveFilepath := filepath.Join(r.backupDir, outputArchive)

	// Save snapshot
	var snapshotSize int64
	if snapshotSize, err = saveSnapshot(cli, snapshotFilepath); err != nil {
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}

	// Save the corresponding static pod resources
	var archiveSize int64
	if archiveSize, err = archiveLatestResources(r.configDir, archiveFilepath); err != nil {
		return fmt.Errorf("archiveLatestResources failed: %w", err)
	}

	if r.terminationLog != "" {
		if err := backuphelpers.WriteTerminationLog(r.terminationLog, snapshotFilepath, snapshotSize, archiveFilepath, archiveSize); err != nil {
			return fmt.Errorf("termiantionLog failed: %w", err)
		}
	}

	return nil
}

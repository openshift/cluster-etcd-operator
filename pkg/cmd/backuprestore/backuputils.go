package backuprestore

import (
	"fmt"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"
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
	klog.Infof("hello from backup server -inside backupOptions backup()  ")
	cli, err := getEtcdClient(r.endpoints)
	if err != nil {
		klog.Errorf("failed to get etcd client [%v]", err)
		return fmt.Errorf("backup: failed to get etcd client: %w", err)
	}
	defer cli.Close()

	klog.Infof("hello from backup server -inside backupOptions backup() - before checkAndCreateDir() ")
	if err := checkAndCreateDir(r.backupDir); err != nil {
		klog.Errorf("hello from backup server -inside backupOptions backup() - inside checkAndCreateDir() [%v] ", err)
		return fmt.Errorf("backup: checkAndCreateDir failed: %w", err)
	}
	klog.Infof("hello from backup server -inside backupOptions backup() - after checkAndCreateDir() ")

	// Trying to match the output file formats with the formats of the current cluster-backup.sh script
	dateString := time.Now().Format("2006-01-02_150405")
	outputArchive := "static_kuberesources_" + dateString + ".tar.gz"
	snapshotOutFile := "snapshot_" + dateString + ".db"

	klog.Infof("hello from backup server -inside backupOptions backup() - before saveSnapshot() ")
	// Save snapshot
	if err := saveSnapshot(cli, filepath.Join(r.backupDir, snapshotOutFile)); err != nil {
		klog.Errorf("hello from backup server -inside backupOptions saveSnapshot() - inside saveSnapshot() [%v] ", err)
		return fmt.Errorf("saveSnapshot failed: %w", err)
	}
	klog.Infof("hello from backup server -inside backupOptions backup() - after saveSnapshot() ")
	// Save the corresponding static pod resources

	klog.Infof("hello from backup server -inside backupOptions backup() - before archiveLatestResources() ")
	if err := archiveLatestResources(r.configDir, filepath.Join(r.backupDir, outputArchive)); err != nil {
		klog.Errorf("hello from backup server -inside backupOptions archiveLatestResources() - inside archiveLatestResources() [%v] ", err)
		return fmt.Errorf("archiveLatestResources failed: %w", err)
	}
	klog.Infof("hello from backup server -inside backupOptions backup() - after archiveLatestResources() ")
	return nil
}

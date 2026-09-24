package backuprestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
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

func backup(r *backupOptions) (err error) {
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

	var message string
	var files []operatorv1alpha1.EtcdBackupFile

	defer func() {
		if r.terminationLog != "" {
			if logErr := writeTerminationLog(r.terminationLog, message, files); logErr != nil {
				if err == nil {
					err = fmt.Errorf("terminationLog failed: %w", logErr)
				}
			}
		}
	}()

	// Save snapshot
	var snapshotSize int64
	snapshotPartialPath := snapshotFilepath + ".part"
	if snapshotSize, err = saveSnapshot(cli, snapshotPartialPath, snapshotFilepath); err != nil {
		// Record partial snapshot path for GC, if present
		message = "snapshot failed"
		if msg, ok := isDiskFull(err); ok {
			message = fmt.Sprintf("%s: %s", message, msg)
		}
		if backupFile, ok := statBackupFile(snapshotPartialPath); ok {
			files = append(files, backupFile)
		}
		return fmt.Errorf("saveSnapshot failed: %w", err)
	} else {
		files = append(files, operatorv1alpha1.EtcdBackupFile{Path: snapshotFilepath, SizeBytes: snapshotSize})
	}

	// Save the corresponding static pod resources
	var archiveSize int64
	if archiveSize, err = archiveLatestResources(r.configDir, archiveFilepath); err != nil {
		// Record partial archive path for GC, if present
		message = "static pod archive failed"
		if msg, ok := isDiskFull(err); ok {
			message = fmt.Sprintf("%s: %s", message, msg)
		}
		if backupFile, ok := statBackupFile(archiveFilepath); ok {
			files = append(files, backupFile)
		}
		return fmt.Errorf("archiveLatestResources failed: %w", err)
	} else {
		files = append(files, operatorv1alpha1.EtcdBackupFile{Path: archiveFilepath, SizeBytes: archiveSize})
	}

	return
}

func statBackupFile(path string) (file operatorv1alpha1.EtcdBackupFile, ok bool) {
	if info, err := os.Stat(path); err != nil {
		return operatorv1alpha1.EtcdBackupFile{Path: path, SizeBytes: info.Size()}, true
	}
	return
}

func writeTerminationLog(terminationLog string, message string, files []operatorv1alpha1.EtcdBackupFile) (retErr error) {
	f, err := os.OpenFile(terminationLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("Error opening termination log: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil && retErr != nil {
			retErr = fmt.Errorf("Error closing termination log: %w", err)
		}
	}()

	termLog := backuphelpers.BackupTerminationLog{Message: message, Files: files}
	if err := json.NewEncoder(f).Encode(termLog); err != nil {
		return fmt.Errorf("Error writing termination log: %w", err)
	}

	return nil
}

func isDiskFull(err error) (string, bool) {
	if errors.Is(err, syscall.ENOSPC) {
		return "no disk space", true
	}
	return "", false
}

package backuprestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

//This backup mimics the functionality of cluster-backup.sh

// Termination log is capped at 4096 bytes by the kubelet
const maxTerminationMessageBytes = 4096

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
			return -1, fmt.Errorf("findTheLatestRevision failed: %w", err)
		}
		paths = append(paths, latestPod)
		klog.Info("\tAdding the latest revision for podName ", podName, ": ", latestPod)
	}

	size, err := createTarball(backupFile, paths, configDir)
	if err != nil {
		return -1, fmt.Errorf("Got error creating the tar archive: %w", err)
	}
	return size, nil
}

func backup(ctx context.Context, r *backupOptions) (err error) {
	cli, err := getEtcdClient(r.endpoints)
	if err != nil {
		return fmt.Errorf("backup: failed to get etcd client: %w", err)
	}
	defer cli.Close()

	if err := checkAndCreateBackupDir(r.backupDir); err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	// Trying to match the output file formats with the formats of the current cluster-backup.sh script
	dateString := time.Now().Format("2006-01-02_150405")
	snapshotFile := "snapshot_" + dateString + ".db"
	snapshotPartialFile := snapshotFile + ".part"
	archiveFile := "static_kuberesources_" + dateString + ".tar.gz"
	snapshotFilepath := filepath.Join(r.backupDir, snapshotFile)
	snapshotPartialFilepath := filepath.Join(r.backupDir, snapshotPartialFile)
	archiveFilepath := filepath.Join(r.backupDir, archiveFile)

	var message string
	var files []operatorv1alpha1.EtcdBackupFile

	defer func() {
		if r.terminationLog != "" {
			if logErr := writeTerminationLog(r.terminationLog, message, files); logErr != nil {
				err = utilerrors.NewAggregate([]error{err, logErr})
			}
		}
	}()

	var backupSnapshot, backupStaticKubeResources bool
	if r.assets == nil {
		backupSnapshot, backupStaticKubeResources = true, true
	} else {
		for _, asset := range r.assets {
			switch asset {
			case string(operatorv1alpha1.EtcdBackupAssetTypeSnapshot):
				backupSnapshot = true
			case string(operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources):
				backupStaticKubeResources = true
			}
		}
	}

	if backupSnapshot {
		// Save snapshot
		if snapshotSize, err := saveSnapshot(ctx, cli, snapshotPartialFilepath, snapshotFilepath); err != nil {
			// Record partial snapshot path for GC, if present
			message = "snapshot failed"
			if msg, ok := isDiskFull(err); ok {
				message = fmt.Sprintf("%s: %s", message, msg)
			}
			if snapshotPartialSize, ok := statBackupFile(snapshotPartialFilepath); ok {
				files = append(files, operatorv1alpha1.EtcdBackupFile{
					AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot,
					Path:      snapshotPartialFile, // stored relative to backup dir
					SizeBytes: snapshotPartialSize,
				})
			}
			return fmt.Errorf("saveSnapshot failed: %w", err)
		} else {
			files = append(files, operatorv1alpha1.EtcdBackupFile{
				AssetType: operatorv1alpha1.EtcdBackupAssetTypeSnapshot,
				Path:      snapshotFile, // stored relative to backup dir
				SizeBytes: snapshotSize,
			})
		}
	}

	if backupStaticKubeResources {
		// Save the corresponding static pod resources
		if archiveSize, err := archiveLatestResources(r.configDir, archiveFilepath); err != nil {
			// Record partial archive path for GC, if present
			message = "static pod archive failed"
			if msg, ok := isDiskFull(err); ok {
				message = fmt.Sprintf("%s: %s", message, msg)
			}
			if archivePartialSize, ok := statBackupFile(archiveFilepath); ok {
				files = append(files, operatorv1alpha1.EtcdBackupFile{
					AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources,
					Path:      archiveFile, // stored relative to backup dir
					SizeBytes: archivePartialSize,
				})
			}
			return fmt.Errorf("archiveLatestResources failed: %w", err)
		} else {
			files = append(files, operatorv1alpha1.EtcdBackupFile{
				AssetType: operatorv1alpha1.EtcdBackupAssetTypeStaticKubeResources,
				Path:      archiveFile, // stored relative to backup dir
				SizeBytes: archiveSize,
			})
		}
	}

	return
}

func statBackupFile(path string) (sizeBytes int64, ok bool) {
	if info, err := os.Stat(path); err == nil {
		return info.Size(), true
	}
	return
}

func writeTerminationLog(terminationLog string, message string, files []operatorv1alpha1.EtcdBackupFile) (retErr error) {
	f, err := os.OpenFile(terminationLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("Error opening termination log: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			retErr = utilerrors.NewAggregate([]error{retErr, fmt.Errorf("Error closing termination log: %w", err)})
		}
	}()

	return encodeTerminationLog(f, message, files)
}

func encodeTerminationLog(w io.Writer, message string, files []operatorv1alpha1.EtcdBackupFile) error {
	termLog := backuphelpers.BackupTerminationLog{Message: message, Files: files}
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(termLog); err != nil || buf.Len() > maxTerminationMessageBytes {
		// Strip message if max length is exceeded or encoding failed.
		// Retaining files in term log is the most critical, message is a nice-to-have.
		klog.Warningf("Dropping message from termination log (encoded %d bytes, err: %v): %s", buf.Len(), err, message)
		termLog.Message = ""
		buf.Reset()
		if err := enc.Encode(termLog); err != nil {
			return fmt.Errorf("error encoding termination log: %w", err)
		}
	}

	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("error writing termination log: %w", err)
	}
	return nil
}

func isDiskFull(err error) (string, bool) {
	if errors.Is(err, syscall.ENOSPC) {
		return "no disk space", true
	}
	return "", false
}

func checkAndCreateBackupDir(dirName string) error {
	// checkAndCreateDir will delete the contents of the directory if not empty.
	// validate that all files are backup files first, otherwise error.
	_, otherFiles, err := backuphelpers.ListBackupFiles(dirName)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("checkAndCreateBackupDir failed: %w", err)
	}
	if len(otherFiles) > 0 {
		return fmt.Errorf("checkAndCreateBackupDir failed: directory is not empty, and contains non-backup files")
	}

	return checkAndCreateDir(dirName)
}

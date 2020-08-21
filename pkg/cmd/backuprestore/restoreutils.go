package backuprestore

import (
	"context"
	"fmt"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"os"
	"path/filepath"
	"time"
)

var (
	static_pod_list = []string{
		"etcd-pod.yaml",
		"kube-apiserver-pod.yaml",
		"kube-controller-manager-pod.yaml",
		"kube-scheduler-pod.yaml",
	}
	staticPodContainers = sets.NewString(
		"etcd",
		"etcdctl",
		"etcd-metrics",
		"kube-controller-manager",
		"kube-apiserver",
		"kube-scheduler",
	)
)

func restore(ctx context.Context, r *restoreOptions) error {

	var (
		assetDir           = filepath.Join(r.configDir, "assets")
		manifestDir        = filepath.Join(r.configDir, "manifests")
		manifestStoppedDir = filepath.Join(assetDir, "manifests-stopped")
		dataDirBackup      = r.dataDir + "-backup"
	)

	// locate snapshot db and static pod resources archive
	snapshotFile, err := findTheLatestRevision(r.backupDir, "snapshot_", false)
	if err != nil {
		return fmt.Errorf("restore: could not find snapshot db: %w", err)
	}
	resourcesArchive, err := findTheLatestRevision(r.backupDir, "static_kuberesources_", false)
	if err != nil {
		return fmt.Errorf("restore: could not find static pod resources: %w", err)
	}

	// Move manifests to manifests-stopped-dir
	klog.Info("moving manifests to manifests-stopped-dir")
	if err := checkAndCreateDir(manifestStoppedDir); err != nil {
		return fmt.Errorf("restore: checkAndCreateDir failed: %w", err)
	}
	for _, podyaml := range static_pod_list {
		podyamlFullpath := filepath.Join(manifestDir, podyaml)
		fileExists, err := fileExists(podyamlFullpath)
		if err != nil {
			return fmt.Errorf("restore: Unexpected error checking file: %s. Error: %w", podyamlFullpath, err)
		}
		// it is fine, if the file doesn't exist
		if fileExists {
			err := os.Rename(podyamlFullpath, filepath.Join(manifestStoppedDir, podyaml))
			if err != nil {
				return fmt.Errorf("restore: attempt to stop %s failed: %w", podyaml, err)
			}
		}
	}

	// Wait for static pods to stop
	klog.Info("waiting for static pods to stop ...")
	if err := waitForStaticPodsToStop(ctx); err != nil {
		return fmt.Errorf("restore: waitForStaticPodsToStop failed %w", err)
	}
	klog.Info("\nall static pods stopped.")

	dataDirMember := filepath.Join(r.dataDir, "member")
	dataDirMemberExists, err := dirExists(dataDirMember)
	if err != nil {
		return fmt.Errorf("restore: Unexpected error checking dir: %s. Error: %w", dataDirMember, err)
	}
	klog.Info("backing up current data-dir before restore")
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
			if err := os.RemoveAll(dataDirBackupMember); err != nil {
				return fmt.Errorf("restore: failed to remove data-dir backup %s failed: %w", dataDirBackupMember, err)
			}
		}
		// Rename data-dir/member to data-dir-backup/member
		if err := os.Rename(dataDirMember, dataDirBackupMember); err != nil {
			return fmt.Errorf("restore: attempt to backup data-dir %s failed: %w", r.dataDir, err)
		}
	}

	// Restore static pod resources
	klog.Info("extracting static-pod-resources")
	if err := extractAllFromTarGz(resourcesArchive, r.configDir); err != nil {
		return fmt.Errorf("restore: attempt to extract static-pod-resources from archive %s failed: %w",
			resourcesArchive, err)
	}

	// Copy snapshot to backupdir
	klog.Infof("copying snapshot.db to %s", dataDirBackup)
	etcdDataDirBackupSnapshot := filepath.Join(dataDirBackup, "snapshot.db")
	if _, err := fileCopy(snapshotFile, etcdDataDirBackupSnapshot); err != nil {
		return fmt.Errorf("restore: attempt to copy snapshot %s failed: %w", snapshotFile, err)
	}

	// Copy restore etcd pod to manifest directory
	klog.Info("copying restore etcd pod to manifest directory")
	if err := extractFileFromTarGzToTargetFile(resourcesArchive, filepath.Join(manifestDir, "etcd-pod.yaml"), "restore-etcd-pod/pod.yaml"); err != nil {
		return fmt.Errorf("restore: attempt to extract restore-etcd-pod/pod.yaml failed: %w", err)
	}

	// Restore remaining static pods
	klog.Info("restoring remaining static pods")
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

func waitForStaticPodsToStop(ctx context.Context) error {
	runtimeClient, runtimeConn, err := getRuntimeCrioClient()
	if err != nil {
		return err
	}
	defer func() {
		if err := runtimeConn.Close(); err != nil {
			klog.Warningf("crioclient: CloseConnection failed %v", err)
		}
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	allStaticPodsStopped := false
	unStoppedPod := ""
	for !allStaticPodsStopped {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}

		allStaticPodsStopped = true
		containersList, err := listContainers(ctx, runtimeClient)
		if err != nil {
			return fmt.Errorf("listing containers: %w", err)
		}

		for _, c := range containersList {
			if staticPodContainers.Has(c.Metadata.Name) {
				allStaticPodsStopped = false
				// If the name is a repeat, just print a dot(.) without newline.
				if c.Metadata.Name == unStoppedPod {
					fmt.Printf(".")
					break
				} else if unStoppedPod != "" {
					fmt.Println("")
				}

				unStoppedPod = c.Metadata.Name
				klog.Infof("... static pod %s is still active", c.Metadata.Name)
				break
			}
		}
	}
	return nil
}

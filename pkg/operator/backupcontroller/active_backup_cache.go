package backupcontroller

import (
	"fmt"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
)

func newActiveBackupCache() activeBackupCache {
	return activeBackupCache{
		backups: map[string]activeBackup{},
		// Limit 1 active Local backup per Node
		local: activeResources{},
		// Limit 1 active PVC backup per PVC
		pvc: activeResources{},
	}
}

// activeBackupCache tracks the resources held by backups that have been started. It persists between syncs so that
// backups started by the controller are still accounted for while the informer caches catch up.
type activeBackupCache struct {
	// map active backups by name
	backups map[string]activeBackup
	// map node to the active local backups running on the node. Should be at most 1 unless something unexpected happens.
	local activeResources
	// map pvc to the active pvc backups running on the pvc. Should be at most 1 unless something unexpected happens.
	pvc activeResources
}

type activeBackup struct {
	nodeName, pvcName string
}

// newActiveBackup returns the resources held by a backup.
func newActiveBackup(backup *operatorv1alpha1.EtcdBackup) activeBackup {
	var nodeName, pvcName string
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		nodeName = backup.Status.NodeName
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		pvcName = backup.Spec.Storage.PVC.Name
	}
	return activeBackup{nodeName: nodeName, pvcName: pvcName}
}

func (abc activeBackupCache) isActive(backupName string) bool {
	_, ok := abc.backups[backupName]
	return ok
}

func (abc *activeBackupCache) add(backup *operatorv1alpha1.EtcdBackup) {
	backupName := backup.Name
	active := newActiveBackup(backup)
	if existing, ok := abc.backups[backupName]; ok {
		// Backup already cached. Make sure nothing has changed.
		if existing.nodeName != active.nodeName {
			if existing.nodeName != "" {
				abc.local.remove(existing.nodeName, backupName)
			}
			if active.nodeName != "" {
				abc.local.add(active.nodeName, backupName)
			}
		}
		if existing.pvcName != active.pvcName {
			if existing.pvcName != "" {
				abc.pvc.remove(existing.pvcName, backupName)
			}
			if active.pvcName != "" {
				abc.pvc.add(active.pvcName, backupName)
			}
		}
	} else {
		if active.nodeName != "" {
			abc.local.add(active.nodeName, backupName)
		}
		if active.pvcName != "" {
			abc.pvc.add(active.pvcName, backupName)
		}
	}
	abc.backups[backupName] = active
}

func (abc *activeBackupCache) remove(backupName string) {
	if activeBackup, ok := abc.backups[backupName]; ok {
		if activeBackup.nodeName != "" {
			abc.local.remove(activeBackup.nodeName, backupName)
		}
		if activeBackup.pvcName != "" {
			abc.pvc.remove(activeBackup.pvcName, backupName)
		}
		delete(abc.backups, backupName)
	}
}

func (abc activeBackupCache) canStart(backup *operatorv1alpha1.EtcdBackup) (ok bool, reason string) {
	switch backup.Spec.Storage.Type {
	case operatorv1alpha1.EtcdBackupStorageTypeLocal:
		if nodeName := backup.Status.NodeName; abc.local.inUse(nodeName) {
			backupNames := make([]string, 0, len(abc.local[nodeName]))
			for name := range abc.local[nodeName] {
				backupNames = append(backupNames, name)
			}
			return false, fmt.Sprintf("Node %s has active backup(s): %v", nodeName, backupNames)
		}
	case operatorv1alpha1.EtcdBackupStorageTypePVC:
		if pvcName := backup.Spec.Storage.PVC.Name; abc.pvc.inUse(pvcName) {
			return false, fmt.Sprintf("PVC %s in use", pvcName)
		}
	}
	return true, ""
}

type activeResources map[string]map[string]struct{}

func (ar activeResources) inUse(name string) bool {
	_, ok := ar[name]
	return ok
}

func (ar activeResources) add(name, backupName string) {
	if backups, ok := ar[name]; ok {
		backups[backupName] = struct{}{}
	} else {
		ar[name] = map[string]struct{}{backupName: {}}
	}
}

func (ar activeResources) remove(name, backupName string) {
	if backups, ok := ar[name]; ok {
		delete(backups, backupName)
		if len(backups) == 0 {
			delete(ar, name)
		}
	}
}

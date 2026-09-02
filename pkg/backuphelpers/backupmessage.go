package backuphelpers

import (
	"encoding/json"
	"fmt"
	"os"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type BackupTerminationLog struct {
	Files []operatorv1alpha1.EtcdBackupFile `json:"files"`
}

func WriteTerminationLog(terminationLog, snapshotFile string, snapshotSize int64, archiveFile string, archiveSize int64) (retErr error) {
	f, err := os.OpenFile(terminationLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("Error opening termination log: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil && retErr != nil {
			retErr = fmt.Errorf("Error closing termination log: %w", err)
		}
	}()

	termLog := BackupTerminationLog{
		Files: []operatorv1alpha1.EtcdBackupFile{
			{Path: snapshotFile, Size: *resource.NewQuantity(snapshotSize, resource.BinarySI)},
			{Path: archiveFile, Size: *resource.NewQuantity(archiveSize, resource.BinarySI)},
		},
	}
	if err := json.NewEncoder(f).Encode(termLog); err != nil {
		return fmt.Errorf("Error writing termination log: %w", err)
	}

	return nil
}

package backuprestore

import (
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

const validSchedule = "0 */2 * * *"

func TestBackupServer_Validate(t *testing.T) {
	testCases := []struct {
		name         string
		backupServer backupServer
		expErr       error
	}{
		{
			"BackupServer is disabled",
			backupServer{enabled: false},
			nil,
		},
		{
			"BackupServer is disabled and invalid schedule",
			backupServer{
				enabled:  false,
				schedule: "invalid schedule",
			},
			nil,
		},
		{
			"BackupServer is disabled and invalid schedule and invalid backup directory",
			backupServer{
				enabled:  false,
				schedule: "invalid schedule",
				backupOptions: backupOptions{
					backupDir: "",
				},
			},
			nil,
		},
		{
			"BackupServer is enabled",
			backupServer{
				enabled: true,
			},
			errors.New("error parsing backup schedule : empty spec string"),
		},
		{
			"BackupServer is enabled and invalid schedule",
			backupServer{
				enabled:  true,
				schedule: "invalid schedule",
			},
			errors.New("error parsing backup schedule invalid schedule"),
		},
		{
			"BackupServer is enabled and valid schedule",
			backupServer{
				enabled:  true,
				schedule: validSchedule,
			},
			nil,
		},
		{
			"BackupServer is enabled and invalid schedule and invalid backup directory",
			backupServer{
				enabled:  true,
				schedule: "invalid schedule",
				backupOptions: backupOptions{
					backupDir: "",
				},
			},
			errors.New("error parsing backup schedule invalid schedule"),
		},
		{
			"BackupServer is enabled and valid schedule and invalid backup directory",
			backupServer{
				enabled:  true,
				schedule: validSchedule,
				backupOptions: backupOptions{
					backupDir: "",
				},
			},
			nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actErr := tc.backupServer.Validate()
			if tc.expErr != nil {
				require.Contains(t, actErr.Error(), tc.expErr.Error())
			} else {
				require.Equal(t, tc.expErr, actErr)
			}
		})
	}
}

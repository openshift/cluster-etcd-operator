package backuprestore

import (
	"context"
	"errors"
	"github.com/robfig/cron"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const validSchedule = "* * * * *"

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

func TestNewBackupServer_scheduleBackup(t *testing.T) {
	srvr := &backupServer{
		timeZone:      "",
		enabled:       true,
		backupOptions: backupOptions{},
	}

	testCases := []struct {
		name       string
		schedule   string
		timeout    time.Duration
		expBackups int
		expErr     error
	}{
		{
			name:       "valid schedule",
			schedule:   validSchedule,
			timeout:    time.Minute * 3,
			expBackups: 3,
			expErr:     nil,
		},
		{
			name:       "invalid schedule",
			schedule:   "invalid schedule",
			timeout:    time.Minute * 3,
			expBackups: 0,
			expErr:     errors.New("Expected exactly 5 fields"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			cronSchedule, err := cron.ParseStandard(tc.schedule)
			if tc.expErr != nil {
				require.Contains(t, err.Error(), tc.expErr.Error())
				return
			}
			require.NoError(t, err)
			srvr.cronSchedule = cronSchedule

			mock := backUpErMock{counter: 0}
			ctxTimeout, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()

			err = srvr.scheduleBackup(ctxTimeout, &mock)
			require.Equal(t, tc.expErr, err)
			require.GreaterOrEqual(t, mock.counter, 3)
		})
	}
}

type backUpErMock struct {
	counter int
}

func (b *backUpErMock) runBackup(opts *backupOptions) error {
	b.counter++
	return nil
}

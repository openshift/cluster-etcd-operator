package backuprestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	"github.com/robfig/cron"
	"github.com/stretchr/testify/require"
)

const (
	validSchedule = "* * * * *"
	localHost     = "localhost"
)

func TestBackupServer_Validate(t *testing.T) {
	testCases := []struct {
		name         string
		backupServer backupServer
		envExist     bool
		expErr       error
	}{
		{
			"BackupServer is disabled",
			backupServer{enabled: false},
			false,
			nil,
		},
		{
			"BackupServer is disabled and invalid schedule",
			backupServer{
				enabled:  false,
				schedule: "invalid schedule",
			},
			false,
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
			false,
			nil,
		},
		{
			"BackupServer is enabled",
			backupServer{
				enabled: true,
			},
			true,
			errors.New("error parsing backup schedule : empty spec string"),
		},
		{
			"BackupServer is enabled and invalid schedule",
			backupServer{
				enabled:  true,
				schedule: "invalid schedule",
			},
			true,
			errors.New("error parsing backup schedule invalid schedule"),
		},
		{
			"BackupServer is enabled and valid schedule",
			backupServer{
				enabled:  true,
				schedule: validSchedule,
				PruneOpts: prune.PruneOpts{
					RetentionType: prune.RetentionTypeNone,
				},
			},
			true,
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
			true,
			errors.New("error parsing backup schedule invalid schedule"),
		},
		{
			"BackupServer is enabled and valid schedule and invalid backup directory",
			backupServer{
				schedule: validSchedule,
				enabled:  true,
				backupOptions: backupOptions{
					backupDir: "",
				},
				PruneOpts: prune.PruneOpts{
					RetentionType: prune.RetentionTypeNone,
				},
			},
			true,
			nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envExist {
				err := os.Setenv(nodeNameEnvVar, localHost)
				require.NoError(t, err)
			}

			actErr := tc.backupServer.Validate()
			if tc.expErr != nil {
				require.Contains(t, actErr.Error(), tc.expErr.Error())
			} else {
				require.Equal(t, tc.expErr, actErr)
			}

			t.Cleanup(func() {
				err := os.Unsetenv(nodeNameEnvVar)
				require.NoError(t, err)
			})
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
		slow       bool
		expBackups int
		expErr     error
	}{
		{
			name:       "valid schedule",
			schedule:   "*/1 * * * * *",
			timeout:    time.Second * 3,
			slow:       false,
			expBackups: 3,
			expErr:     nil,
		},
		{
			name:       "slow valid schedule",
			schedule:   "*/2 * * * * *",
			timeout:    time.Second * 8,
			slow:       true,
			expBackups: 3,
			expErr:     nil,
		},
		{
			name:       "invalid schedule",
			schedule:   "invalid schedule",
			timeout:    time.Minute * 3,
			expBackups: 0,
			expErr:     errors.New("Expected 5 to 6 fields"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			cronSchedule, err := cron.Parse(tc.schedule)
			if tc.expErr != nil {
				require.Contains(t, err.Error(), tc.expErr.Error())
				return
			}
			require.NoError(t, err)
			srvr.cronSchedule = cronSchedule

			mock := backupRunnerMock{counter: 0}
			if tc.slow {
				mock.slow = true
				mock.delay = time.Second * 3
			}
			ctxTimeout, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()

			err = srvr.scheduleBackup(ctxTimeout, &mock)
			require.Equal(t, tc.expErr, err)
			require.GreaterOrEqual(t, mock.counter, tc.expBackups)
		})
	}
}

func TestBackupServer_validateNameNode(t *testing.T) {
	testCases := []struct {
		name          string
		inputNodeName string
		envExist      bool
		expErr        error
	}{
		{
			name:          "env var exist",
			inputNodeName: localHost,
			envExist:      true,
			expErr:        nil,
		},
		{
			name:     "env var not exist",
			envExist: false,
			expErr:   fmt.Errorf("[NODE_NAME] environment variable is empty"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envExist {
				err := os.Setenv(nodeNameEnvVar, tc.inputNodeName)
				require.NoError(t, err)
			}

			b := &backupServer{}
			err := b.validateNameNode()
			require.Equal(t, tc.expErr, err)
			require.Equal(t, b.nodeName, tc.inputNodeName)

			t.Cleanup(func() {
				err := os.Unsetenv(nodeNameEnvVar)
				require.NoError(t, err)
			})
		})
	}
}

func TestBackupServer_constructEnvVars(t *testing.T) {
	b := &backupServer{
		nodeName: localHost,
	}

	err := b.constructEnvVars()
	require.NoError(t, err)

	expEtcdKey := "/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-localhost.key"
	act := os.Getenv(etcdCtlKeyName)
	require.Equal(t, expEtcdKey, act)

	expEtcdCert := "/etc/kubernetes/static-pod-certs/secrets/etcd-all-certs/etcd-peer-localhost.crt"
	act = os.Getenv(etcdCtlCertName)
	require.Equal(t, expEtcdCert, act)

	expEtcdCACert := "/etc/kubernetes/static-pod-certs/configmaps/etcd-all-bundles/server-ca-bundle.crt"
	act = os.Getenv(etcdCtlCACertName)
	require.Equal(t, expEtcdCACert, act)
}

type backupRunnerMock struct {
	counter int
	slow    bool
	delay   time.Duration
}

func (b *backupRunnerMock) runBackup(backupOpts *backupOptions, pruneOpts *prune.PruneOpts) error {
	if b.slow {
		time.Sleep(b.delay)
	}

	b.counter++
	return nil
}

package backuphelpers

import (
	"fmt"
	"testing"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"

	"github.com/stretchr/testify/require"
)

const (
	schedule = "0 */2 * * *"
	timezone = "GMT"
)

func TestBackupConfig_ToArgs(t *testing.T) {
	testCases := []struct {
		name     string
		cr       backupv1alpha1.EtcdBackupSpec
		enabled  bool
		expected string
	}{
		{
			"enabled",
			createEtcdBackupSpec(timezone, schedule),
			true,
			"\t- backup-server\n\t- --enabled=true\n\t- --timezone=GMT\n\t- --schedule=0 */2 * * *",
		},
		{
			"disabled",
			createEtcdBackupSpec(timezone, schedule),
			false,
			"\t- backup-server\n\t- --timezone=GMT\n\t- --schedule=0 */2 * * *",
		},
		{
			"empty schedule disabled",
			createEtcdBackupSpec(timezone, ""),
			false,
			"\t- backup-server\n\t- --timezone=GMT",
		},
		{
			"empty schedule enabled",
			createEtcdBackupSpec(timezone, ""),
			true,
			"\t- backup-server\n\t- --enabled=true\n\t- --timezone=GMT",
		},

		{
			"empty timezone disabled",
			createEtcdBackupSpec("", schedule),
			false,
			"\t- backup-server\n\t- --schedule=0 */2 * * *",
		},
		{
			"empty timezone enabled",
			createEtcdBackupSpec("", schedule),
			true,
			"\t- backup-server\n\t- --enabled=true\n\t- --schedule=0 */2 * * *",
		},

		{
			"empty timezone and schedule disabled",
			createEtcdBackupSpec("", ""),
			false,
			"\t- backup-server",
		},
		{
			"empty timezone and schedule enabled",
			createEtcdBackupSpec("", ""),
			true,
			"\t- backup-server\n\t- --enabled=true",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewDisabledBackupConfig(tc.cr, tc.enabled)
			act := c.ArgString()
			fmt.Println(act)
			require.Equal(t, tc.expected, act)
		})
	}
}

func createEtcdBackupSpec(timezone, schedule string) backupv1alpha1.EtcdBackupSpec {
	return backupv1alpha1.EtcdBackupSpec{
		Schedule: schedule,
		TimeZone: timezone,
	}
}

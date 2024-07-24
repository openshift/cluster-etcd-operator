package backuphelpers

import (
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
			"    - backup-server\n    - --enabled=true\n    - --timezone=GMT\n    - --schedule=0 */2 * * *",
		},
		{
			"disabled",
			createEtcdBackupSpec(timezone, schedule),
			false,
			"    - backup-server\n    - --enabled=false\n    - --timezone=GMT\n    - --schedule=0 */2 * * *",
		},
		{
			"empty schedule disabled",
			createEtcdBackupSpec(timezone, ""),
			false,
			"    - backup-server\n    - --enabled=false\n    - --timezone=GMT",
		},
		{
			"empty schedule enabled",
			createEtcdBackupSpec(timezone, ""),
			true,
			"    - backup-server\n    - --enabled=true\n    - --timezone=GMT",
		},

		{
			"empty timezone disabled",
			createEtcdBackupSpec("", schedule),
			false,
			"    - backup-server\n    - --enabled=false\n    - --schedule=0 */2 * * *",
		},
		{
			"empty timezone enabled",
			createEtcdBackupSpec("", schedule),
			true,
			"    - backup-server\n    - --enabled=true\n    - --schedule=0 */2 * * *",
		},

		{
			"empty timezone and schedule disabled",
			createEtcdBackupSpec("", ""),
			false,
			"    - backup-server\n    - --enabled=false",
		},
		{
			"empty timezone and schedule enabled",
			createEtcdBackupSpec("", ""),
			true,
			"    - backup-server\n    - --enabled=true",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			c := NewDisabledBackupConfig()
			c.backupCR = tc.cr
			if tc.enabled {
				c.enabled = true
			}

			act := c.ArgString()

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

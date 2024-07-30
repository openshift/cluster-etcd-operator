package backuphelpers

import (
	"testing"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"

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
			testutils.CreateEtcdBackupSpec(timezone, schedule),
			true,
			"    - backup-server\n    - --enabled=true\n    - --timezone=GMT\n    - --schedule=0 */2 * * *",
		},
		{
			"disabled",
			testutils.CreateEtcdBackupSpec(timezone, schedule),
			false,
			"    - backup-server\n    - --enabled=true\n    - --timezone=GMT\n    - --schedule=0 */2 * * *",
		},
		{
			"empty schedule disabled",
			testutils.CreateEtcdBackupSpec(timezone, ""),
			false,
			"    - backup-server\n    - --enabled=true\n    - --timezone=GMT",
		},
		{
			"empty schedule enabled",
			testutils.CreateEtcdBackupSpec(timezone, ""),
			true,
			"    - backup-server\n    - --enabled=true\n    - --timezone=GMT",
		},

		{
			"empty timezone disabled",
			testutils.CreateEtcdBackupSpec("", schedule),
			false,
			"    - backup-server\n    - --enabled=true\n    - --schedule=0 */2 * * *",
		},
		{
			"empty timezone enabled",
			testutils.CreateEtcdBackupSpec("", schedule),
			true,
			"    - backup-server\n    - --enabled=true\n    - --schedule=0 */2 * * *",
		},

		{
			"empty timezone and schedule disabled",
			testutils.CreateEtcdBackupSpec("", ""),
			false,
			"    - backup-server\n    - --enabled=false",
		},
		{
			"empty timezone and schedule enabled",
			testutils.CreateEtcdBackupSpec("", ""),
			true,
			"    - backup-server\n    - --enabled=true",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			c := NewDisabledBackupConfig()
			c.SetBackupSpec(tc.cr)
			if tc.enabled {
				c.enabled = true
			}

			act := c.ArgString()

			require.Equal(t, tc.expected, act)
		})
	}
}

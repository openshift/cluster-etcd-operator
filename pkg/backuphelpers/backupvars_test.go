package backuphelpers

import (
	"testing"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestBackupConfig_ToArgs(t *testing.T) {
	input := backupv1alpha1.EtcdBackupSpec{
		Schedule: "0 */2 * * *",
		TimeZone: "GMT",
	}

	expected := "- --timezone=GMT\n- --schedule=0 */2 * * *"

	c := NewBackupConfig(input)
	act := c.ArgString()
	require.Equal(t, expected, act)
}

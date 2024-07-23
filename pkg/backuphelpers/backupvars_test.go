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

	expected := "\t\t- --schedule=0 */2 * * *\n\t\t- --timezone=GMT"

	c := NewDisabledBackupConfig(input)
	act := c.ArgString()
	require.Equal(t, expected, act)
}

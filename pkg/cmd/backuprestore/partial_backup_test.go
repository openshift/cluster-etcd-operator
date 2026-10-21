package backuprestore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRestoreDiscoveryExcludesPartialBackups(test *testing.T) {
	test.Parallel()
	for _, prefix := range []string{"snapshot_", "static_kuberesources_"} {
		test.Run(prefix, func(test *testing.T) {
			test.Parallel()
			directory := test.TempDir()
			partial := filepath.Join(directory, "partial_"+prefix+"2026-01-02")
			require.NoError(test, os.WriteFile(partial, []byte("partial fixture"), 0600))
			_, err := findTheLatestRevision(directory, prefix, false)
			require.Error(test, err)
			complete := filepath.Join(directory, prefix+"2026-01-01")
			require.NoError(test, os.WriteFile(complete, []byte("complete fixture"), 0600))
			selected, err := findTheLatestRevision(directory, prefix, false)
			require.NoError(test, err)
			require.Equal(test, complete, selected)
		})
	}
}

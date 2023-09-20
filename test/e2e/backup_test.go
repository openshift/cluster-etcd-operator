package e2e

import (
	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"
	"regexp"
	"strings"
	"testing"
)

func requireBackupFilesFound(t *testing.T, name string, files []string) {
	// a successful backup will look like this:
	// ./backup-backup-happy-path-2023-08-03_152313
	// ./backup-backup-happy-path-2023-08-03_152313/static_kuberesources_2023-08-03_152316__POSSIBLY_DIRTY__.tar.gz
	// ./backup-backup-happy-path-2023-08-03_152313/snapshot_2023-08-03_152316__POSSIBLY_DIRTY__.db ]

	// we assert that there are always at least two files:
	tarMatchFound := false
	snapMatchFound := false
	for _, file := range files {
		matchesTar, err := regexp.MatchString(`\./backup-`+name+`-.*.tar.gz`, file)
		require.NoError(t, err)
		if matchesTar {
			klog.Infof("Found matching kube resources: %s", file)
			tarMatchFound = true
		}

		matchesSnap, err := regexp.MatchString(`\./backup-`+name+`-.*/snapshot_.*.db`, file)
		require.NoError(t, err)
		if matchesSnap {
			klog.Infof("Found matching snapshot: %s", file)
			snapMatchFound = true
		}
	}

	require.Truef(t, tarMatchFound, "expected tarfile for backup: %s, found files: %v ", name, files)
	require.Truef(t, snapMatchFound, "expected snapshot for backup: %s, found files: %v ", name, files)
}

// Golang 1.20 carry

// CutPrefix returns s without the provided leading prefix string
// and reports whether it found the prefix.
// If s doesn't start with prefix, CutPrefix returns s, false.
// If prefix is the empty string, CutPrefix returns s, true.
func CutPrefix(s, prefix string) (after string, found bool) {
	if !strings.HasPrefix(s, prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

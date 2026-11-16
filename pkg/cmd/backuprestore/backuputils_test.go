package backuprestore

import (
	"bytes"
	"strings"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/stretchr/testify/require"
)

func TestTerminationLogMaxLength(t *testing.T) {
	dateString := time.Now().Format("2006-01-02_150405")
	outputArchive := "static_kuberesources_" + dateString + ".tar.gz"
	snapshotOutFile := "snapshot_" + dateString + ".db"
	files := []operatorv1.EtcdBackupFile{
		{
			AssetType: operatorv1.EtcdBackupAssetTypeSnapshot,
			Path:      snapshotOutFile,
			SizeBytes: 1000000000000,
		},
		{
			AssetType: operatorv1.EtcdBackupAssetTypeStaticKubeResources,
			Path:      outputArchive,
			SizeBytes: 1000000000000,
		},
	}

	testCases := []struct {
		name        string
		message     string
		isTruncated bool
	}{
		{
			name:        "empty",
			message:     "",
			isTruncated: false,
		},
		{
			name:        "short",
			message:     "short error message",
			isTruncated: false,
		},
		{
			name:        "long ascii",
			message:     strings.Repeat("some really long error message with: ", 1000),
			isTruncated: true,
		},
		{
			name:        "heavily escaped",
			message:     strings.Repeat("\"\\\n", 2000),
			isTruncated: true,
		},
		{
			name:        "multibyte rune",
			message:     strings.Repeat("😀", 2000),
			isTruncated: true,
		},
	}

	buf := &bytes.Buffer{}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			err := encodeTerminationLog(buf, tc.message, files)
			require.NoError(t, err)
			require.LessOrEqual(t, buf.Len(), 4096, "expected encoded message to be less than 4096 bytes")

			termLog, err := backuphelpers.ParseTerminationMessage(buf.String())
			require.NoError(t, err)

			if tc.isTruncated {
				require.Equal(t, "", termLog.Message)
			} else {
				require.Equal(t, tc.message, termLog.Message)
			}
			// Files are left intact
			require.Equal(t, files, termLog.Files)
		})
	}
}

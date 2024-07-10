package backuprestore

import (
	"errors"
	"testing"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	fake "github.com/openshift/client-go/config/clientset/versioned/fake"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stretchr/testify/require"
)

func TestBackupNoConfig_extractBackupSpecs(t *testing.T) {
	testCases := []struct {
		name       string
		backupName string
		schedule   string
		expErr     error
	}{
		{
			name:       "empty input",
			backupName: "",
			schedule:   "",
			expErr:     errors.New("no backup CRDs exist, found"),
		},
		{
			name:       "non default backup",
			backupName: "test-backup",
			schedule:   "20 4 * * *",
			expErr:     errors.New("could not find default backup CR"),
		},
		{
			name:       "default backup",
			backupName: "default",
			schedule:   "10 8 * 7 *",
			expErr:     nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// arrange
			var operatorFake *fake.Clientset
			backup := createBackupObject(tc.backupName, tc.schedule)

			if backup != nil {
				operatorFake = fake.NewSimpleClientset([]runtime.Object{backup}...)
			} else {
				operatorFake = fake.NewSimpleClientset()
			}

			// act
			b := &backupNoConfig{}
			err := b.extractBackupSpecs(operatorFake.ConfigV1alpha1())

			// assert
			if tc.expErr != nil {
				require.ErrorContains(t, err, tc.expErr.Error())
			} else {
				require.Equal(t, tc.expErr, err)
				require.Equal(t, tc.schedule, b.schedule)
				require.Equal(t, getRetentionPolicy(), b.retention)
			}
		})
	}
}

func createBackupObject(backupName, schedule string) *backupv1alpha1.Backup {
	if backupName == "" {
		return nil
	}
	return &backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: backupName},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule:        schedule,
				RetentionPolicy: getRetentionPolicy(),
				TimeZone:        "UTC",
				PVCName:         "backup-happy-path-pvc"}}}
}

func getRetentionPolicy() backupv1alpha1.RetentionPolicy {
	return backupv1alpha1.RetentionPolicy{
		RetentionType:   backupv1alpha1.RetentionTypeNumber,
		RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}}
}

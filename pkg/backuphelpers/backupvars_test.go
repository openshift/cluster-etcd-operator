package backuphelpers

import (
	"testing"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	prune "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	"github.com/stretchr/testify/require"
)

const (
	schedule = "0 */2 * * *"
	timezone = "GMT"
)

func TestBackupConfig_ToArgs(t *testing.T) {
	testCases := []struct {
		name     string
		cr       *backupv1alpha1.EtcdBackupSpec
		expected string
	}{
		{
			"backup spec with timezone and schedule",
			createEtcdBackupSpec(timezone, schedule),
			"    args:\n    - --enabled=true\n    - --timezone=GMT\n    - --schedule=0 */2 * * *",
		},
		{
			"backup spec with timezone and empty schedule",
			createEtcdBackupSpec(timezone, ""),
			"    args:\n    - --enabled=true\n    - --timezone=GMT",
		},
		{
			"backup spec with empty timezone and schedule",
			createEtcdBackupSpec("", schedule),
			"    args:\n    - --enabled=true\n    - --schedule=0 */2 * * *",
		},
		{
			"backup spec with timezone and schedule and retention number",
			withRetentionNumberThreeBackups(createEtcdBackupSpec(timezone, schedule)),
			"    args:\n    - --enabled=true\n    - --timezone=GMT\n    - --schedule=0 */2 * * *\n    - --type=RetentionNumber\n    - --maxNumberOfBackups=3",
		},
		{
			"backup spec with timezone and schedule and retention size",
			withRetentionSizeOneGB(createEtcdBackupSpec(timezone, schedule)),
			"    args:\n    - --enabled=true\n    - --timezone=GMT\n    - --schedule=0 */2 * * *\n    - --type=RetentionSize\n    - --maxSizeOfBackupsGb=1",
		},
		{
			"backup spec with empty timezone and empty schedule",
			nil,
			"    args:\n    - --enabled=false",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			c := NewDisabledBackupConfig()
			c.SetBackupSpec(tc.cr)

			act := c.ArgString()

			require.Equal(t, tc.expected, act)
		})
	}
}

func TestBackupConfig_ProbeString(t *testing.T) {
	scenarios := []struct {
		name     string
		cr       *backupv1alpha1.EtcdBackupSpec
		expected string
	}{
		{
			name:     "backup spec with timezone and schedule",
			cr:       createEtcdBackupSpec(timezone, schedule),
			expected: "    livenessProbe:\n      httpGet:\n        path: healthz\n        port: 8000\n        scheme: HTTPS\n      timeoutSeconds: 60\n      periodSeconds: 5\n      successThreshold: 1\n      failureThreshold: 5",
		},
		{
			"backup spec with empty timezone and empty schedule",
			nil,
			"",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			c := NewDisabledBackupConfig()
			c.SetBackupSpec(scenario.cr)

			act := c.ProbeString()

			require.Equal(t, scenario.expected, act)
		})
	}
}

func createEtcdBackupSpec(timezone, schedule string) *backupv1alpha1.EtcdBackupSpec {
	return &backupv1alpha1.EtcdBackupSpec{
		Schedule: schedule,
		TimeZone: timezone,
	}
}

func withRetentionNumberThreeBackups(b *backupv1alpha1.EtcdBackupSpec) *backupv1alpha1.EtcdBackupSpec {
	b.RetentionPolicy.RetentionType = prune.RetentionTypeNumber
	b.RetentionPolicy.RetentionNumber = &backupv1alpha1.RetentionNumberConfig{
		MaxNumberOfBackups: 3,
	}
	return b
}

func withRetentionSizeOneGB(b *backupv1alpha1.EtcdBackupSpec) *backupv1alpha1.EtcdBackupSpec {
	b.RetentionPolicy.RetentionType = prune.RetentionTypeSize
	b.RetentionPolicy.RetentionSize = &backupv1alpha1.RetentionSizeConfig{
		MaxSizeOfBackupsGb: 1,
	}
	return b
}

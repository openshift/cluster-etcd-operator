package backuphelpers

import (
	"testing"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/stretchr/testify/require"
)

const (
	schedule = "0 */2 * * *"
)

func TestBackupConfig_ToArgs(t *testing.T) {
	testCases := []struct {
		name     string
		cr       *operatorv1alpha1.EtcdBackupPolicySpec
		expected string
	}{
		{
			"backup spec with schedule",
			createEtcdBackupPolicySpec(schedule),
			"    args:\n    - --enabled=true\n    - --schedule=0 */2 * * *",
		},
		{
			"backup spec with empty schedule",
			createEtcdBackupPolicySpec(""),
			"    args:\n    - --enabled=true",
		},
		{
			"backup spec with schedule and retention number",
			withRetentionNumberThreeBackups(createEtcdBackupPolicySpec(schedule)),
			"    args:\n    - --enabled=true\n    - --schedule=0 */2 * * *\n    - --type=RetentionNumber\n    - --maxNumberOfBackups=3",
		},
		{
			"backup spec with timezone and schedule and retention size",
			withRetentionSizeOneGB(createEtcdBackupPolicySpec(schedule)),
			"    args:\n    - --enabled=true\n    - --schedule=0 */2 * * *\n    - --type=RetentionSize\n    - --maxSizeOfBackupsGb=1",
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

func TestBackupConfig_ToArgList(t *testing.T) {
	testCases := []struct {
		name     string
		cr       *operatorv1alpha1.EtcdBackupPolicySpec
		expected []string
	}{
		{
			"backup spec schedule",
			createEtcdBackupPolicySpec(schedule),
			[]string{
				"--enabled=true",
				"--schedule=0 */2 * * *",
			},
		},
		{
			"backup spec with empty schedule",
			createEtcdBackupPolicySpec(""),
			[]string{
				"--enabled=true",
			},
		},
		{
			"backup spec with schedule and retention number",
			withRetentionNumberThreeBackups(createEtcdBackupPolicySpec(schedule)),
			[]string{
				"--enabled=true",
				"--schedule=0 */2 * * *",
				"--type=RetentionNumber",
				"--maxNumberOfBackups=3",
			},
		},
		{
			"backup spec with timezone and schedule and retention size",
			withRetentionSizeOneGB(createEtcdBackupPolicySpec(schedule)),
			[]string{
				"--enabled=true",
				"--schedule=0 */2 * * *",
				"--type=RetentionSize",
				"--maxSizeOfBackupsGb=1",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			c := NewDisabledBackupConfig()
			c.SetBackupSpec(tc.cr)
			require.Equal(t, tc.expected, c.ArgList())
		})
	}
}

func createEtcdBackupPolicySpec(schedule string) *operatorv1alpha1.EtcdBackupPolicySpec {
	return &operatorv1alpha1.EtcdBackupPolicySpec{
		Schedule: schedule,
	}
}

func withRetentionNumberThreeBackups(b *operatorv1alpha1.EtcdBackupPolicySpec) *operatorv1alpha1.EtcdBackupPolicySpec {
	b.RetentionRules = append(b.RetentionRules, operatorv1alpha1.EtcdBackupPolicyRetentionRule{
		Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity, MaxQuantity: 3,
	})
	return b
}

func withRetentionSizeOneGB(b *operatorv1alpha1.EtcdBackupPolicySpec) *operatorv1alpha1.EtcdBackupPolicySpec {
	b.RetentionRules = append(b.RetentionRules, operatorv1alpha1.EtcdBackupPolicyRetentionRule{
		Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxSize, MaxSize: resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
	})
	return b
}

package ceohelpers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCalculateFailureThreshold(t *testing.T) {
	scenarios := []struct {
		name          string
		baseThreshold int
		quotaGiB      int32
		expected      int
	}{
		{
			name:          "default quota 8GB",
			baseThreshold: 20,
			quotaGiB:      8,
			expected:      20,
		},
		{
			name:          "zero quota defaults to 8GB",
			baseThreshold: 20,
			quotaGiB:      0,
			expected:      20,
		},
		{
			name:          "double quota 16GB",
			baseThreshold: 20,
			quotaGiB:      16,
			expected:      40,
		},
		{
			name:          "maximum quota 32GB",
			baseThreshold: 20,
			quotaGiB:      32,
			expected:      80,
		},
		{
			name:          "readiness probe base at 8GB",
			baseThreshold: 3,
			quotaGiB:      8,
			expected:      3,
		},
		{
			name:          "readiness probe at 16GB",
			baseThreshold: 3,
			quotaGiB:      16,
			expected:      6,
		},
		{
			name:          "readiness probe at 32GB",
			baseThreshold: 3,
			quotaGiB:      32,
			expected:      12,
		},
		{
			name:          "minimum quota 8GB with small base",
			baseThreshold: 1,
			quotaGiB:      8,
			expected:      1,
		},
		{
			name:          "rounding test - 12GB quota",
			baseThreshold: 20,
			quotaGiB:      12,
			expected:      30, // 20 * (12/8) = 30
		},
		{
			name:          "rounding test - 10GB quota",
			baseThreshold: 3,
			quotaGiB:      10,
			expected:      4, // 3 * (10/8) = 3.75, rounds to 4
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			result := calculateFailureThreshold(scenario.baseThreshold, scenario.quotaGiB)
			require.Equal(t, scenario.expected, result)
		})
	}
}

func TestCalculateFailureThresholdMinimum(t *testing.T) {
	// Ensure that the function always returns at least 1
	result := calculateFailureThreshold(0, 8)
	require.Equal(t, 1, result, "calculateFailureThreshold should return at least 1")
}

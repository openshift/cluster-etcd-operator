package tools

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetJobName(t *testing.T) {
	tests := []struct {
		name           string
		jobType        JobType
		nodeName       *string
		expectedName   string // only used when nodeName is nil
		expectedMaxLen int
	}{
		// All job types without node name
		{
			name:           "Auth job without node name",
			jobType:        JobTypeAuth,
			nodeName:       nil,
			expectedName:   "tnf-auth-job",
			expectedMaxLen: MaxK8sResourceNameLength,
		},
		{
			name:           "Setup job without node name",
			jobType:        JobTypeSetup,
			nodeName:       nil,
			expectedName:   "tnf-setup-job",
			expectedMaxLen: MaxK8sResourceNameLength,
		},
		{
			name:           "AfterSetup job without node name",
			jobType:        JobTypeAfterSetup,
			nodeName:       nil,
			expectedName:   "tnf-after-setup-job",
			expectedMaxLen: MaxK8sResourceNameLength,
		},
		{
			name:           "Fencing job without node name",
			jobType:        JobTypeFencing,
			nodeName:       nil,
			expectedName:   "tnf-fencing-job",
			expectedMaxLen: MaxK8sResourceNameLength,
		},
		{
			name:           "UpdateSetup job without node name",
			jobType:        JobTypeUpdateSetup,
			nodeName:       nil,
			expectedName:   "tnf-update-setup-job",
			expectedMaxLen: MaxK8sResourceNameLength,
		},
		// All job types with short node name
		{
			name:           "Auth job with short node name",
			jobType:        JobTypeAuth,
			nodeName:       strPtr("master-0"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "Setup job with short node name",
			jobType:        JobTypeSetup,
			nodeName:       strPtr("worker-1"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "AfterSetup job with short node name",
			jobType:        JobTypeAfterSetup,
			nodeName:       strPtr("master-2"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "Fencing job with short node name",
			jobType:        JobTypeFencing,
			nodeName:       strPtr("worker-0"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "UpdateSetup job with short node name",
			jobType:        JobTypeUpdateSetup,
			nodeName:       strPtr("master-1"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		// All job types with long node name requiring truncation
		{
			name:           "Auth job with long node name requiring truncation",
			jobType:        JobTypeAuth,
			nodeName:       strPtr("worker-0.very-long-cluster-name.region.cloud-provider.example.com"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "Setup job with long node name requiring truncation",
			jobType:        JobTypeSetup,
			nodeName:       strPtr("master-0.very-long-cluster-name.region.cloud-provider.example.com"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "AfterSetup job with long node name requiring truncation",
			jobType:        JobTypeAfterSetup,
			nodeName:       strPtr("worker-1.very-long-cluster-name.region.cloud-provider.example.com"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "Fencing job with long node name requiring truncation",
			jobType:        JobTypeFencing,
			nodeName:       strPtr("master-2.extremely-long-domain-name-that-exceeds-limits.example.com"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
		{
			name:           "UpdateSetup job with long node name requiring truncation",
			jobType:        JobTypeUpdateSetup,
			nodeName:       strPtr("worker-2.extremely-long-domain-name-that-exceeds-limits.example.com"),
			expectedMaxLen: MaxK8sResourceNameLength - PodSuffixBuffer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.jobType.GetJobName(tt.nodeName)

			expectedPrefix := fmt.Sprintf("tnf-%s-job", tt.jobType.GetSubCommand())

			if tt.nodeName == nil {
				// Exact match for jobs without node name
				require.Equal(t, tt.expectedName, result,
					"Expected job name %q, got %q", tt.expectedName, result)
			} else {
				// For jobs with node name, verify prefix format
				require.True(t, strings.HasPrefix(result, expectedPrefix+"-"),
					"Expected job name to start with %q, got %q", expectedPrefix+"-", result)
			}

			// Always check length constraint
			require.LessOrEqual(t, len(result), tt.expectedMaxLen,
				"Job name %q exceeds max length %d (got %d)", result, tt.expectedMaxLen, len(result))
		})
	}
}

func TestShortenNodeName(t *testing.T) {
	tests := []struct {
		name       string
		nodeName   string
		maxLength  int
		checkExact bool
		expected   string
	}{
		{
			name:       "Short node name with standard pattern fits within limit",
			nodeName:   "worker-0",
			maxLength:  20,
			checkExact: true,
			expected:   "worker-0-72f9309a",
		},
		{
			name:       "Master node name with standard pattern",
			nodeName:   "master-1",
			maxLength:  20,
			checkExact: true,
			expected:   "master-1-64736551",
		},

		{
			name:       "Long node name with standard pattern gets truncated",
			nodeName:   "worker-0.very-long-cluster-name.region.example.com",
			maxLength:  20,
			checkExact: false,
		},
		{
			name:       "Non-standard node name without pattern",
			nodeName:   "custom-node-without-number",
			maxLength:  20,
			checkExact: false,
		},
		{
			name:       "Very short maxLength forces hash truncation",
			nodeName:   "worker-0.example.com",
			maxLength:  8,
			checkExact: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shortenNodeName(tt.nodeName, tt.maxLength)

			// Always check length constraint
			require.LessOrEqual(t, len(result), tt.maxLength,
				"Shortened name %q exceeds max length %d (got %d)", result, tt.maxLength, len(result))

			if tt.checkExact {
				require.Equal(t, tt.expected, result,
					"Expected shortened name %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestShortenNodeNameDeterminism(t *testing.T) {
	// Verify that the same input always produces the same output
	nodeName := "worker-0.very-long-cluster-name.region.example.com"
	maxLength := 25

	result1 := shortenNodeName(nodeName, maxLength)
	result2 := shortenNodeName(nodeName, maxLength)

	require.Equal(t, result1, result2,
		"shortenNodeName should be deterministic, got %q and %q", result1, result2)
}

func TestShortenNodeNameUniqueness(t *testing.T) {
	// Verify that different node names produce different shortened names
	maxLength := 25

	names := []string{
		"worker-0.cluster-a.example.com",
		"worker-0.cluster-b.example.com",
		"worker-1.cluster-a.example.com",
	}

	results := make(map[string]string)
	for _, name := range names {
		result := shortenNodeName(name, maxLength)
		// Check no collision
		for originalName, existingResult := range results {
			require.NotEqual(t, existingResult, result,
				"Collision detected: %q and %q both shortened to %q", originalName, name, result)
		}
		results[name] = result
	}
}

func TestShortenNodeNameDNSCompliance(t *testing.T) {
	// DNS labels must:
	// - contain only lowercase alphanumeric or '-'
	// - start with alphanumeric
	// - end with alphanumeric
	// - be at most 63 characters
	dnsLabelRegex := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

	tests := []struct {
		name      string
		nodeName  string
		maxLength int
	}{
		{
			name:      "Standard worker node",
			nodeName:  "worker-0",
			maxLength: 25,
		},
		{
			name:      "Long node name with standard pattern",
			nodeName:  "worker-0.very-long-cluster-name.region.example.com",
			maxLength: 20,
		},
		{
			name:      "Non-standard node name",
			nodeName:  "custom-node-without-number",
			maxLength: 20,
		},
		{
			name:      "Node name that would truncate at hyphen",
			nodeName:  "worker-0.cluster-name.example.com",
			maxLength: 17, // would cut "worker-0" to "worker-" without sanitization
		},
		{
			name:      "Very short maxLength",
			nodeName:  "worker-0.example.com",
			maxLength: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shortenNodeName(tt.nodeName, tt.maxLength)

			require.LessOrEqual(t, len(result), tt.maxLength,
				"Result %q exceeds max length %d", result, tt.maxLength)

			require.Regexp(t, dnsLabelRegex, result,
				"Result %q is not a valid DNS label", result)
		})
	}
}

func TestSanitizeDNSLabel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "No hyphens to trim",
			input:    "worker0",
			expected: "worker0",
		},
		{
			name:     "Trailing hyphen",
			input:    "worker-",
			expected: "worker",
		},
		{
			name:     "Leading hyphen",
			input:    "-worker",
			expected: "worker",
		},
		{
			name:     "Both leading and trailing hyphens",
			input:    "-worker-",
			expected: "worker",
		},
		{
			name:     "Multiple trailing hyphens",
			input:    "worker--",
			expected: "worker",
		},
		{
			name:     "Only hyphens",
			input:    "---",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeDNSLabel(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}

func strPtr(s string) *string {
	return &s
}

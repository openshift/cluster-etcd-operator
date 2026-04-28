package ensureenv

import (
	"os"
	"testing"
)

func TestEnsureEnv(t *testing.T) {
	tests := []struct {
		name     string
		envVars  map[string]string
		args     []string
		expected string
	}{
		{
			name:     "missing node-ip",
			args:     []string{},
			expected: "--node-ip must be set to a non-empty value",
		},
		{
			name: "env var not set",
			args: []string{
				"--node-ip=10.0.0.1",
				"--check-set-and-not-empty=MISSING_VAR",
			},
			expected: "environment variable MISSING_VAR must be set",
		},
		{
			name: "env var empty",
			args: []string{
				"--node-ip=10.0.0.1",
				"--check-set-and-not-empty=EMPTY_VAR",
			},
			envVars:  map[string]string{"EMPTY_VAR": ""},
			expected: "environment variable EMPTY_VAR must not be empty",
		},
		{
			name: "env var set and not empty",
			args: []string{
				"--node-ip=10.0.0.1",
				"--check-set-and-not-empty=GOOD_VAR",
			},
			envVars: map[string]string{"GOOD_VAR": "value"},
		},
		{
			name: "env var equals node ip",
			args: []string{
				"--node-ip=10.0.0.1",
				"--check-equals-node-ip=IP_VAR",
			},
			envVars: map[string]string{"IP_VAR": "10.0.0.1"},
		},
		{
			name: "env var does not equal node ip",
			args: []string{
				"--node-ip=10.0.0.1",
				"--check-equals-node-ip=IP_VAR",
			},
			envVars:  map[string]string{"IP_VAR": "10.0.0.2"},
			expected: "expected IP_VAR to be 10.0.0.1, got 10.0.0.2",
		},
		{
			name: "ipv6 with brackets matches bare",
			args: []string{
				"--node-ip=fd00::1",
				"--check-equals-node-ip=IP_VAR",
			},
			envVars: map[string]string{"IP_VAR": "[fd00::1]"},
		},
		{
			name: "bare ipv6 matches bracketed node-ip",
			args: []string{
				"--node-ip=[fd00::1]",
				"--check-equals-node-ip=IP_VAR",
			},
			envVars: map[string]string{"IP_VAR": "fd00::1"},
		},
		{
			name: "malformed ipv6 double brackets rejected",
			args: []string{
				"--node-ip=fd00::1",
				"--check-equals-node-ip=IP_VAR",
			},
			envVars:  map[string]string{"IP_VAR": "[[fd00::1]]"},
			expected: "expected IP_VAR to be fd00::1, got [[fd00::1]]",
		},
		{
			name: "full bash-equivalent invocation",
			args: []string{
				"--node-ip=10.0.0.1",
				"--check-set-and-not-empty=ETCD_URL_HOST,ETCD_NAME,NODE_IP_VAR",
				"--check-equals-node-ip=NODE_IP_VAR,ETCD_URL_HOST",
			},
			envVars: map[string]string{
				"ETCD_URL_HOST": "10.0.0.1",
				"ETCD_NAME":     "etcd-member-0",
				"NODE_IP_VAR":   "10.0.0.1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				os.Setenv(k, v)
			}
			defer func() {
				for k := range tt.envVars {
					os.Unsetenv(k)
				}
			}()

			c := NewEnsureEnvCommand()
			c.SetArgs(tt.args)
			err := c.Execute()

			if tt.expected != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.expected)
				}
				if err.Error() != tt.expected {
					t.Fatalf("expected error %q, got %q", tt.expected, err.Error())
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
		})
	}
}

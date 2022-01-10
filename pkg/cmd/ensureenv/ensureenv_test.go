package ensureenv

import (
	"os"
	"strings"
	"testing"
)

type testdata struct {
	name     string
	toSet    map[string]string
	args     []string
	expected string
}

func runTests(t *testing.T, tests []testdata) {
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for k, v := range test.toSet {
				os.Setenv(k, v)
			}

			out := &strings.Builder{}
			c := NewEnsureEnvCommand(out)
			c.SetArgs(test.args)
			if err := c.Execute(); err != nil {
				t.Errorf("Got unexpected cobra error: %s", err)
			}

			if err := out.String(); err != "" {
				if err != test.expected {
					t.Errorf("Expected error: %s. Got error: %s", test.expected, err)
				}
			} else if test.expected != "" {
				t.Errorf("Expected error: %s. Got no error.", test.expected)
			}

			for k := range test.toSet {
				os.Unsetenv(k)
			}
		})
	}
}

func TestEnsure(t *testing.T) {
	TestEnsureValidate(t)
	TestEnsureRun(t)
}

func TestEnsureValidate(t *testing.T) {
	tests := []testdata{
		{
			name: "TestNotSet",
			args: []string{
				"--check-set-and-not-empty=EV_NOT_SET",
			},
			expected: "the value of environment variable EV_NOT_SET must be set.",
		},
		{
			name: "TestEmpty",
			args: []string{
				"--check-set-and-not-empty=EV_EMPTY",
			},
			toSet: map[string]string{
				"EV_EMPTY": "",
			},
			expected: "the value of environment variable EV_EMPTY must not be empty.",
		},
		{
			name: "TestUnsetNodeIP",
			args: []string{
				"--allow-invalid-node-ip=false",
			},
			expected: "since --allow-invalid-node-ip is not set, --node-ip must be set to a non-empty value.",
		},
		{
			name: "TestEmptyNodeIP",
			args: []string{
				"--node-ip=",
				"--allow-invalid-node-ip=false",
			},
			expected: "since --allow-invalid-node-ip is not set, --node-ip must be set to a non-empty value.",
		},
		{
			name: "TestUnsetCurrentRev",
			args: []string{
				"--node-ip=127.0.0.1",
			},
			expected: "--current-revision-node-ip must be set to a non-empty value.",
		},
		{
			name: "TestUnsetCurrentRev",
			args: []string{
				"--node-ip=127.0.0.1",
				"--current-revision-node-ip=",
			},
			expected: "--current-revision-node-ip must be set to a non-empty value.",
		},
	}

	runTests(t, tests)
}

func TestEnsureRun(t *testing.T) {
	tests := []testdata{
		{
			name: "TestNodeIPEqualsCurrentRev",
			args: []string{
				"--node-ip=equal",
				"--allow-invalid-node-ip=false",
				"--current-revision-node-ip=equal",
			},
			expected: "",
		},
		{
			name: "TestInvalidNodeIPNotAllow",
			args: []string{
				"--node-ip=invalid_value",
				"--allow-invalid-node-ip=false",
				"--current-revision-node-ip=127.0.0.1",
			},
			expected: "since --allow-invalid-node-ip is not set, node-ip and current-revision-node-ip must be equal.",
		},
		{
			name: "TestCurrentRevNoMatch",
			args: []string{
				"--allow-invalid-node-ip=true",
				"--current-revision-node-ip=no_match",
			},
			expected: "failed to find ip address on network interfaces: no_match",
		},
		{
			name: "TestCurrentRevMatch",
			args: []string{
				"--allow-invalid-node-ip=true",
				"--current-revision-node-ip=127.0.0.1",
			},
			expected: "",
		},
	}

	runTests(t, tests)
}

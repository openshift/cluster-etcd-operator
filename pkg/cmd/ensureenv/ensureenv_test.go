package ensureenv

import (
	"os"
	"strings"
	"testing"
)

type testdata struct {
	toSet    map[string]string
	args     []string
	expected string
}

func runTests(t *testing.T, tests map[string]testdata) {
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
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

func TestEnsureValidate(t *testing.T) {
	tests := map[string]testdata{
		"TestNotSet": {
			args: []string{
				"--check-set-and-not-empty=EV_NOT_SET",
			},
			expected: "The value of environment variable EV_NOT_SET must be set.",
		},
		"TestEmpty": {
			args: []string{
				"--check-set-and-not-empty=EV_EMPTY",
			},
			toSet: map[string]string{
				"EV_EMPTY": "",
			},
			expected: "The value of environment variable EV_EMPTY must not be empty.",
		},
		"TestDefaultIPEVEqual": {
			args: []string{
				"--ip-ev-equals=EV_NOT_EQUAL",
				"--allow-invalid-ip-ev-val=false",
			},
			toSet: map[string]string{
				"NODE_IP":      "equal",
				"EV_NOT_EQUAL": "not equal",
			},
			expected: "The value of environment variables NODE_IP, EV_NOT_EQUAL must be equal. Were: \"equal\", \"not equal\"",
		},
		"TestIPEVEqual": {
			args: []string{
				"--ip-ev=NOT_DEFAULT",
				"--ip-ev-equals=EV_NOT_EQUAL",
				"--allow-invalid-ip-ev-val=false",
			},
			toSet: map[string]string{
				"NOT_DEFAULT":  "equal",
				"EV_NOT_EQUAL": "not equal",
			},
			expected: "The value of environment variables NOT_DEFAULT, EV_NOT_EQUAL must be equal. Were: \"equal\", \"not equal\"",
		},
		"TestAllowInvalidNoFallback": {
			args: []string{
				"--allow-invalid-ip-ev-val=true",
				"--fallback-ip-ev=",
			},
			expected: "Since --allow-invalid-ip-ev-val is true, --fallback-ip-ev must be provided.",
		},
		"TestAllowInvalidEmptyFallback": {
			args: []string{
				"--allow-invalid-ip-ev-val=true",
				"--fallback-ip-ev=EMPTY_FALLBACK",
			},
			toSet: map[string]string{
				"EMPTY_FALLBACK": "",
			},
			expected: "Since --allow-invalid-ip-ev-val is true, the value of the environment variable provided by --fallback-ip-ev [EMPTY_FALLBACK] must not be empty.",
		},
		"TestDisallowInvalidNotSet": {
			args: []string{
				"--allow-invalid-ip-ev-val=false",
				"--ip-ev-equals=EV_EQUAL",
			},
			toSet: map[string]string{
				"EV_EQUAL": "equal",
			},
			expected: "The value of environment variables NODE_IP, EV_EQUAL must be equal. Were: \"\", \"equal\"",
		},
		"TestDisallowInvalidEmpty": {
			args: []string{
				"--allow-invalid-ip-ev-val=false",
				"--ip-ev-equals=EV_EQUAL",
			},
			toSet: map[string]string{
				"NODE_IP":  "",
				"EV_EQUAL": "equal",
			},
			expected: "The value of environment variables NODE_IP, EV_EQUAL must be equal. Were: \"\", \"equal\"",
		},
		"TestSuccess": {
			args: []string{
				"--allow-invalid-ip-ev-val=false",
				"--ip-ev-equals=EV_EQUAL",
			},
			toSet: map[string]string{
				"NODE_IP":  "equal",
				"EV_EQUAL": "equal",
			},
			expected: "",
		},
	}

	runTests(t, tests)
}

func TestEnsureRun(t *testing.T) {
	tests := map[string]testdata{
		"TestValidFallback": {
			args: []string{
				"--ip-ev=NODE_IP",
				"--allow-invalid-ip-ev-val=true",
				"--fallback-ip-ev=EV_FALLBACK",
			},
			toSet: map[string]string{
				"EV_FALLBACK": "127.0.0.1",
			},
			expected: "",
		},
		"TestInvalidFallback": {
			args: []string{
				"--ip-ev=NODE_IP",
				"--allow-invalid-ip-ev-val=true",
				"--fallback-ip-ev=EV_FALLBACK",
			},
			toSet: map[string]string{
				"EV_FALLBACK": "garbage",
			},
			expected: "Failed to find any ip addresses in network interfaces that match the value of EV_FALLBACK",
		},
	}

	runTests(t, tests)
}

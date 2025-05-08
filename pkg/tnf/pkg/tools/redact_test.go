package tools

import (
	"fmt"
	"testing"
)

func TestRedactPasswords(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "no passwords",
			input:    "No sensitive data here.",
			expected: "No sensitive data here.",
		},
		{
			name:     "verbose password arg with dashes, position middle",
			input:    "command --safe 42 --password my-password123 --not-relevant foo",
			expected: fmt.Sprintf("command --safe 42 --password %s --not-relevant foo", replaceWith),
		},
		{
			name:     "short password arg with dashes, position middle",
			input:    "command -s 42 -p my-password123 --not-relevant foo",
			expected: fmt.Sprintf("command -s 42 -p %s --not-relevant foo", replaceWith),
		},
		{
			name:     "password arg with equal, position middle",
			input:    "command safe=42 password=my-password123 not-relevant=foo",
			expected: fmt.Sprintf("command safe=42 password=%s not-relevant=foo", replaceWith),
		},
		{
			name:     "verbose password arg with dashes, position last",
			input:    "command --safe 42 --password my-password123",
			expected: fmt.Sprintf("command --safe 42 --password %s", replaceWith),
		},
		{
			name:     "short password arg with dashes, position last",
			input:    "command -s 42 -p my-password123",
			expected: fmt.Sprintf("command -s 42 -p %s", replaceWith),
		},
		{
			name:     "password arg with equal, position last",
			input:    "command safe=42 password=my-password123",
			expected: fmt.Sprintf("command safe=42 password=%s", replaceWith),
		},
		{
			name:     "password in json",
			input:    ", \"name\": \"password\", \"value\": \"password\"}, ",
			expected: fmt.Sprintf(", \"name\": \"password\", \"value\": \"%s\"}, ", replaceWith),
		},
		{
			name:     "password in CIB",
			input:    " name=\"password\" value=\"password\"/>",
			expected: fmt.Sprintf(" name=\"password\" value=\"%s\"/>", replaceWith),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactPasswords(tt.input)
			if got != tt.expected {
				t.Errorf("RedactPasswords(%q)\n  got: %q\n  want: %q", tt.input, got, tt.expected)
			}
		})
	}
}

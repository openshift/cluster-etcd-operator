package updatesetup

import (
	"strings"
	"testing"
)

// Keep the FQDN and command-input validation coverage from the pre-generation runner.
func TestValidCommandNode(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		nodeName string
		valid    bool
	}{
		{"short name", "master-0", true},
		{"single character", "a", true},
		{"numeric leading", "0-master", true},
		{"FQDN", "master-0.example.test", true},
		{"63 character name", strings.Repeat("a", 63), true},
		{"64 character name", strings.Repeat("a", 64), true},
		{"64 character component", strings.Repeat("a", 64) + ".example.test", true},
		{"253 character name", strings.Repeat("a", 253), true},
		{"253 character dotted name", strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("a", 61), true},
		{"empty", "", false},
		{"leading dot", ".master-0", false},
		{"trailing dot", "master-0.", false},
		{"consecutive dots", "master-0..example.test", false},
		{"leading hyphen", "-master-0", false},
		{"trailing hyphen", "master-0-", false},
		{"component leading hyphen", "master-0.-example.test", false},
		{"component trailing hyphen", "master-0.example-.test", false},
		{"uppercase", "Master-0.example.test", false},
		{"underscore", "master_0.example.test", false},
		{"leading space", " master-0", false},
		{"trailing space", "master-0 ", false},
		{"embedded space", "master 0", false},
		{"tab", "master\t0", false},
		{"newline", "master-0\n", false},
		{"option", "--force", false},
		{"semicolon", "master-0;true", false},
		{"pipe", "master-0|true", false},
		{"ampersand", "master-0&true", false},
		{"redirection", "master-0>file", false},
		{"substitution", "master-0$(true)", false},
		{"backticks", "master-0`true`", false},
		{"single quote", "master-'0'", false},
		{"double quote", "master-\"0\"", false},
		{"slash", "master/0", false},
		{"backslash", "master\\0", false},
		{"wildcard", "master-*", false},
		{"254 character name", strings.Repeat("a", 254), false},
		{"254 character dotted name", strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("a", 62), false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := validCommandNode(testCase.nodeName); got != testCase.valid {
				t.Fatalf("validCommandNode(%q) = %t, want %t", testCase.nodeName, got, testCase.valid)
			}
		})
	}
}

package updatesetup

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
)

func TestAddNodeBackToCluster(t *testing.T) {
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
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := addNodeBackToCluster(ctx, nil, testCase.nodeName, "master-1", config.ClusterConfig{})
			if err == nil {
				t.Fatal("expected validation or canceled command error")
			}
			wantPrefix := fmt.Sprintf("invalid node name %q:", testCase.nodeName)
			if testCase.valid {
				wantPrefix = fmt.Sprintf("failed to add missing node %s:", testCase.nodeName)
			}
			if !strings.HasPrefix(err.Error(), wantPrefix) {
				t.Fatalf("got error %q, want prefix %q", err, wantPrefix)
			}
		})
	}
}

func TestCorosyncNodesToRecover(t *testing.T) {
	for _, tc := range []struct {
		name, online, offline, currentNode, otherNode, missing, remove string
		wantErr                                                        bool
	}{
		{"no nodes", "", "", "master-1", "master-0", "", "", true},
		{"missing node", "master-1", "", "master-1", "master-0", "master-0", "", false},
		{"missing node from master-0", "master-0", "", "master-0", "master-1", "master-1", "", false},
		{"unexpected Corosync name", "other-host", "", "master-0", "master-1", "", "", true},
		{"both online", "master-0 master-1", "", "master-1", "master-0", "", "", false},
		{"stopped node", "master-1", "master-0", "master-1", "master-0", "", "master-0", false},
		{"both offline", "", "master-0 master-1", "master-1", "master-0", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := fmt.Sprintf("Corosync Nodes:\n Online: %s\n Offline: %s\n", tc.online, tc.offline)
			missing, offline, err := corosyncNodesToRecover(status, tc.currentNode, tc.otherNode)
			if missing != tc.missing || offline != tc.remove || (err != nil) != tc.wantErr {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, error=%t)", missing, offline, err, tc.missing, tc.remove, tc.wantErr)
			}
		})
	}
}

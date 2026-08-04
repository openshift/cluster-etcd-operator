package updatesetup

import (
	"fmt"
	"testing"
)

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

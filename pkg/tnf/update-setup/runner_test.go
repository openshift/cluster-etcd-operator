package updatesetup

import (
	"fmt"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
)

func TestCorosyncNodesToRecover(t *testing.T) {
	for _, tc := range []struct {
		name, online, offline, missing, remove string
		wantErr                                bool
	}{
		{"no nodes", "", "", "", "", true},
		{"missing node", "master-1", "", "master-0", "", false},
		{"both online", "master-0 master-1", "", "", "", false},
		{"stopped node", "master-1", "master-0", "", "master-0", false},
		{"both offline", "", "master-0 master-1", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := fmt.Sprintf("Corosync Nodes:\n Online: %s\n Offline: %s\n", tc.online, tc.offline)
			missing, offline, err := corosyncNodesToRecover(status, config.ClusterConfig{NodeName1: "master-0", NodeName2: "master-1"})
			if missing != tc.missing || offline != tc.remove || (err != nil) != tc.wantErr {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, error=%t)", missing, offline, err, tc.missing, tc.remove, tc.wantErr)
			}
		})
	}
}

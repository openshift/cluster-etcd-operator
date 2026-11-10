package pcs

import (
	"testing"
)

func TestAlertConfigs(t *testing.T) {
	if len(AlertConfigs) != 2 {
		t.Fatalf("expected 2 alert configs, got %d", len(AlertConfigs))
	}

	expectedConfigs := []AlertConfig{
		{ID: "tnf-taint-alert", Path: "/var/lib/pacemaker/alerts/tnf-taint-alert.sh", SelectXML: "<select_fencing/>", ExpectFencing: true},
		{ID: "tnf-untaint-alert", Path: "/var/lib/pacemaker/alerts/tnf-untaint-alert.sh", SelectXML: "<select_nodes/>", ExpectNodes: true},
	}

	for i, expected := range expectedConfigs {
		if AlertConfigs[i].ID != expected.ID {
			t.Errorf("AlertConfigs[%d].ID = %q, want %q", i, AlertConfigs[i].ID, expected.ID)
		}
		if AlertConfigs[i].Path != expected.Path {
			t.Errorf("AlertConfigs[%d].Path = %q, want %q", i, AlertConfigs[i].Path, expected.Path)
		}
		if AlertConfigs[i].SelectXML != expected.SelectXML {
			t.Errorf("AlertConfigs[%d].SelectXML = %q, want %q", i, AlertConfigs[i].SelectXML, expected.SelectXML)
		}
		if AlertConfigs[i].ExpectFencing != expected.ExpectFencing {
			t.Errorf("AlertConfigs[%d].ExpectFencing = %v, want %v", i, AlertConfigs[i].ExpectFencing, expected.ExpectFencing)
		}
		if AlertConfigs[i].ExpectNodes != expected.ExpectNodes {
			t.Errorf("AlertConfigs[%d].ExpectNodes = %v, want %v", i, AlertConfigs[i].ExpectNodes, expected.ExpectNodes)
		}
	}
}

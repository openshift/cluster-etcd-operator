package pcs

import (
	"testing"
)

func TestAlertConfigs(t *testing.T) {
	if len(alertConfigs) != 2 {
		t.Fatalf("expected 2 alert configs, got %d", len(alertConfigs))
	}

	expectedConfigs := []alertConfig{
		{id: "tnf-taint-alert", path: "/var/lib/pacemaker/alerts/tnf-taint-alert.sh", selectXML: "<select_fencing/>"},
		{id: "tnf-untaint-alert", path: "/var/lib/pacemaker/alerts/tnf-untaint-alert.sh", selectXML: "<select_nodes/>"},
	}

	for i, expected := range expectedConfigs {
		if alertConfigs[i].id != expected.id {
			t.Errorf("alertConfigs[%d].id = %q, want %q", i, alertConfigs[i].id, expected.id)
		}
		if alertConfigs[i].path != expected.path {
			t.Errorf("alertConfigs[%d].path = %q, want %q", i, alertConfigs[i].path, expected.path)
		}
		if alertConfigs[i].selectXML != expected.selectXML {
			t.Errorf("alertConfigs[%d].selectXML = %q, want %q", i, alertConfigs[i].selectXML, expected.selectXML)
		}
	}
}

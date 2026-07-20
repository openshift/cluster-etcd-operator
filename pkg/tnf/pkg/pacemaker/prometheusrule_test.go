package pacemaker

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type prometheusRuleManifest struct {
	Spec struct {
		Groups []ruleGroup `yaml:"groups"`
	} `yaml:"spec"`
}

type ruleGroup struct {
	Name  string `yaml:"name"`
	Rules []rule `yaml:"rules"`
}

type rule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func repoRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
}

func loadTNFAlertGroup(t *testing.T) []rule {
	t.Helper()
	manifestPath := filepath.Join(repoRoot(), "manifests", "0000_90_etcd-operator_03_prometheusrule.yaml")

	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err, "should read PrometheusRule manifest")

	var manifest prometheusRuleManifest
	require.NoError(t, yaml.Unmarshal(data, &manifest), "should parse YAML")

	for _, g := range manifest.Spec.Groups {
		if g.Name == "tnf-pacemaker.rules" {
			return g.Rules
		}
	}
	t.Fatal("tnf-pacemaker.rules group not found in generated PrometheusRule manifest")
	return nil
}

type expectedAlert struct {
	Name     string
	Expr     string
	For      string
	Severity string
}

var expectedAlerts = []expectedAlert{
	{"TNFNodeCountMismatch", "tnf_cluster_node_count_as_expected == 0", "5m", "critical"},
	{"TNFClusterInMaintenance", "tnf_cluster_in_service == 0", "2m", "warning"},
	{"TNFNodeOffline", "tnf_node_online == 0", "2m", "critical"},
	{"TNFNodeFencingUnavailable", "tnf_node_fencing_available == 0", "5m", "critical"},
	{"TNFNodeFencingDegraded", "tnf_node_fencing_healthy == 0 and tnf_node_fencing_available == 1 and on(node) tnf_node_in_service == 1", "10m", "warning"},
	{"TNFNodeUnclean", "tnf_node_clean == 0", "5m", "critical"},
	{"TNFNodeInMaintenance", "tnf_node_in_service == 0 and tnf_cluster_in_service == 1", "2m", "warning"},
	{"TNFNodeStandby", "tnf_node_active == 0", "5m", "warning"},
	{"TNFResourceStopped", "tnf_resource_started == 0 and on(node) tnf_node_active == 1", "5m", "critical"},
	{"TNFResourceFailed", "tnf_resource_operational == 0 and on(node) tnf_node_active == 1", "2m", "critical"},
	{"TNFResourceUnmanaged", "tnf_resource_managed == 0 and on(node) tnf_node_in_service == 1 and on(node) tnf_node_active == 1", "5m", "warning"},
	{"TNFResourceDisabled", "tnf_resource_enabled == 0 and on(node) tnf_node_active == 1", "5m", "warning"},
}

func TestTNFAlerts_Count(t *testing.T) {
	rules := loadTNFAlertGroup(t)
	require.Len(t, rules, 12, "tnf-pacemaker.rules should contain exactly 12 alerts")
}

func TestTNFAlerts_Definitions(t *testing.T) {
	rules := loadTNFAlertGroup(t)

	rulesByName := make(map[string]rule, len(rules))
	for _, r := range rules {
		rulesByName[r.Alert] = r
	}

	for _, exp := range expectedAlerts {
		t.Run(exp.Name, func(t *testing.T) {
			r, ok := rulesByName[exp.Name]
			require.True(t, ok, "alert %s should exist", exp.Name)
			require.Equal(t, exp.Expr, r.Expr, "expression mismatch")
			require.Equal(t, exp.For, r.For, "for duration mismatch")
			require.Equal(t, exp.Severity, r.Labels["severity"], "severity mismatch")
		})
	}
}

func TestTNFAlerts_SeverityCounts(t *testing.T) {
	rules := loadTNFAlertGroup(t)

	var critical, warning int
	for _, r := range rules {
		switch r.Labels["severity"] {
		case "critical":
			critical++
		case "warning":
			warning++
		default:
			t.Errorf("unexpected severity %q on alert %s", r.Labels["severity"], r.Alert)
		}
	}

	require.Equal(t, 6, critical, "should have 6 critical alerts")
	require.Equal(t, 6, warning, "should have 6 warning alerts")
}

func TestTNFAlerts_Annotations(t *testing.T) {
	rules := loadTNFAlertGroup(t)

	for _, r := range rules {
		t.Run(r.Alert, func(t *testing.T) {
			require.NotEmpty(t, r.Annotations["summary"], "should have summary annotation")
			require.NotEmpty(t, r.Annotations["description"], "should have description annotation")
			require.NotEmpty(t, r.Annotations["runbook_url"], "should have runbook_url annotation")
			require.Contains(t, r.Annotations["runbook_url"],
				"https://github.com/openshift/runbooks/blob/master/alerts/cluster-etcd-operator/"+r.Alert+".md",
				"runbook_url should point to correct alert runbook")
		})
	}
}

func TestTNFAlerts_AllNamesStartWithTNF(t *testing.T) {
	rules := loadTNFAlertGroup(t)

	for _, r := range rules {
		require.Regexp(t, `^TNF[A-Z]`, r.Alert, "alert names should start with TNF prefix")
	}
}

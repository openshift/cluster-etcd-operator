// Command extract_prometheus_rules writes a promtool-compatible rules file
// containing only the groups from an OpenShift PrometheusRule manifest.
package main

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

type prometheusRule struct {
	Spec struct {
		Groups []any `json:"groups"`
	} `json:"spec"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <prometheusrule.yaml> <out-rules.yaml>\n", os.Args[0])
		os.Exit(2)
	}
	inPath, outPath := os.Args[1], os.Args[2]

	raw, err := os.ReadFile(inPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}

	var pr prometheusRule
	if err := yaml.Unmarshal(raw, &pr); err != nil {
		fmt.Fprintf(os.Stderr, "unmarshal: %v\n", err)
		os.Exit(1)
	}
	if len(pr.Spec.Groups) == 0 {
		fmt.Fprintf(os.Stderr, "no rule groups found in %s\n", inPath)
		os.Exit(1)
	}

	out, err := yaml.Marshal(map[string]any{"groups": pr.Spec.Groups})
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
}

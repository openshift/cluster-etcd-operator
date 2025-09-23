package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"

	"github.com/spf13/cobra"

	// The import below is necessary to ensure that the cluster etcd operator tests are registered with the extension.
	_ "github.com/openshift/cluster-etcd-operator/test/extended"
)

func main() {
	registry := e.NewRegistry()
	ext := e.NewExtension("openshift", "payload", "cluster-etcd-operator")

	// Suite: conformance/parallel (fast, parallel-safe)
	ext.AddSuite(e.Suite{
		Name:    "openshift/cluster-etcd-operator/conformance/parallel",
		Parents: []string{"openshift/conformance/parallel"},
		Qualifiers: []string{
			`!(name.contains("[Serial]") || name.contains("[Slow]"))`,
		},
	})

	// Suite: conformance/serial (explicitly serial tests)
	ext.AddSuite(e.Suite{
		Name:    "openshift/cluster-etcd-operator/conformance/serial",
		Parents: []string{"openshift/conformance/serial"},
		Qualifiers: []string{
			`name.contains("[Serial]")`,
		},
	})

	// Suite: optional/slow (long-running tests)
	ext.AddSuite(e.Suite{
		Name:    "openshift/cluster-etcd-operator/optional/slow",
		Parents: []string{"openshift/optional/slow"},
		Qualifiers: []string{
			`name.contains("[Slow]")`,
		},
	})

	// Suite: all (includes everything)
	ext.AddSuite(e.Suite{
		Name: "openshift/cluster-etcd-operator/all",
	})

	specs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		panic(fmt.Sprintf("couldn't build extension test specs from ginkgo: %+v", err.Error()))
	}

	// Ensure [Disruptive] tests are also [Serial] (for any future tests that might need this)
	specs = specs.Walk(func(spec *et.ExtensionTestSpec) {
		if strings.Contains(spec.Name, "[Disruptive]") && !strings.Contains(spec.Name, "[Serial]") {
			spec.Name = strings.ReplaceAll(
				spec.Name,
				"[Disruptive]",
				"[Serial][Disruptive]",
			)
		}
	})

	// Preserve original-name labels for renamed tests
	specs = specs.Walk(func(spec *et.ExtensionTestSpec) {
		for label := range spec.Labels {
			if strings.HasPrefix(label, "original-name:") {
				parts := strings.SplitN(label, "original-name:", 2)
				if len(parts) > 1 {
					spec.OriginalName = parts[1]
				}
			}
		}
	})

	// Ignore obsolete tests
	ext.IgnoreObsoleteTests(
	// "[sig-etcd] <test name here>",
	)

	// Initialize environment before running any tests
	specs.AddBeforeAll(func() {
		// do stuff
	})

	ext.AddSpecs(specs)
	registry.Register(ext)

	root := &cobra.Command{
		Long: "Cluster Etcd Operator Tests Extension",
	}

	root.AddCommand(cmd.DefaultExtensionCommands(registry)...)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

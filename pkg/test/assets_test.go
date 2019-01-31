package test

import (
	"testing"

	"github.com/ghodss/yaml"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render"
	"github.com/openshift/library-go/pkg/assets"
)

func TestYamlCorrectness(t *testing.T) {
	readAllYaml("../../manifests/", t)
	readAllYaml("../../bindata/", t)
}

func readAllYaml(path string, t *testing.T) {
	manifests, err := assets.New(path, render.TemplateData{}, assets.OnlyYaml)
	if err != nil {
		t.Errorf("Unexpected error reading manifests from %s: %v", path, err)
	}
	t.Logf("Found %d manifests in %s", len(manifests), path)
	for _, m := range manifests {
		contents := make(map[string]interface{})
		t.Logf("Checking %s...", m.Name)
		if err := yaml.Unmarshal(m.Data, &contents); err != nil {
			t.Errorf("Unexpected error unmarshaling %s: %v", m.Name, err)
		}
	}
}

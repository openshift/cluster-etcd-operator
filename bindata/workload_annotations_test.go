package bindata

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	workloadAnnotationKey   = "target.workload.openshift.io/management"
	workloadAnnotationValue = "PreferredDuringScheduling"
)

// workloadAnnotation is the expected JSON structure of the workload annotation value.
type workloadAnnotation struct {
	Effect string `json:"effect"`
}

// workloadKinds lists Kubernetes resource kinds that schedule pods and therefore
// require the workload partitioning annotation for CPU management.
var workloadKinds = map[string]bool{
	"DaemonSet":  true,
	"Deployment": true,
	"Job":        true,
	"CronJob":    true,
	"Pod":        true,
}

// exemptManifests lists files that are intentionally exempt from the workload
// annotation requirement, along with the reason for the exemption.
var exemptManifests = map[string]string{
	"etcd/restore-pod.yaml":        "ephemeral restore pod",
	"etcd/quorum-restore-pod.yaml": "ephemeral restore pod",
	// TODO: remove this exemption once PR #1706 merges (OCPBUGS-123168).
	"tnfdeployment/cert-watcher-daemonset.yaml": "annotation added by PR #1706 (OCPBUGS-123168)",
}

// manifest is a minimal representation of a Kubernetes resource used to
// extract the kind, metadata, and the pod template annotation path.
type manifest struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   objectMeta      `json:"metadata"`
	Spec       json.RawMessage `json:"spec"`
}

type objectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Annotations map[string]string `json:"annotations"`
}

type podTemplateSpec struct {
	Metadata objectMeta `json:"metadata"`
}

type jobSpec struct {
	Template podTemplateSpec `json:"template"`
}

type cronJobSpec struct {
	JobTemplate struct {
		Spec jobSpec `json:"spec"`
	} `json:"jobTemplate"`
}

type workloadSpec struct {
	Template podTemplateSpec `json:"template"`
}

// TestWorkloadAnnotations walks all embedded YAML manifests and verifies that
// every workload-type resource carries the target.workload.openshift.io/management
// annotation required for CPU partitioning on OpenShift nodes.
func TestWorkloadAnnotations(t *testing.T) {
	err := fs.WalkDir(f, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		ext := filepath.Ext(path)
		// Only process YAML files, but skip Go-template YAML files which
		// contain template directives that break standard YAML parsing.
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if strings.HasSuffix(path, ".gotpl.yaml") {
			return nil
		}

		// Check if this file is explicitly exempted.
		if reason, ok := exemptManifests[path]; ok {
			t.Logf("skipping exempt manifest %s: %s", path, reason)
			return nil
		}

		data, err := fs.ReadFile(f, path)
		if err != nil {
			t.Logf("skipping %s: cannot read file: %v", path, err)
			return nil
		}

		var m manifest
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Logf("skipping %s: cannot unmarshal: %v", path, err)
			return nil
		}

		if !workloadKinds[m.Kind] {
			return nil
		}

		annotations, err := getPodAnnotations(m)
		if err != nil {
			t.Logf("skipping %s (%s): cannot extract pod annotations: %v", path, m.Kind, err)
			return nil
		}

		value, ok := annotations[workloadAnnotationKey]
		if !ok {
			t.Errorf("%s (%s %s/%s) is missing annotation %q on its pod template; "+
				"add: target.workload.openshift.io/management: '{\"effect\": \"PreferredDuringScheduling\"}'",
				path, m.Kind, m.Metadata.Namespace, m.Metadata.Name, workloadAnnotationKey)
			return nil
		}

		var wa workloadAnnotation
		if err := json.Unmarshal([]byte(value), &wa); err != nil {
			t.Errorf("%s (%s %s/%s) has invalid JSON in annotation %q: %v; "+
				"expected: '{\"effect\": \"PreferredDuringScheduling\"}'",
				path, m.Kind, m.Metadata.Namespace, m.Metadata.Name, workloadAnnotationKey, err)
			return nil
		}
		if wa.Effect != workloadAnnotationValue {
			t.Errorf("%s (%s %s/%s) has unexpected effect %q in annotation %q; "+
				"expected: %q",
				path, m.Kind, m.Metadata.Namespace, m.Metadata.Name, wa.Effect, workloadAnnotationKey, workloadAnnotationValue)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("error walking embedded filesystem: %v", err)
	}
}

// getPodAnnotations extracts the annotations map from the pod-level metadata
// for each workload kind. For Pods the top-level metadata is used; for other
// kinds the pod template spec metadata is used.
func getPodAnnotations(m manifest) (map[string]string, error) {
	switch m.Kind {
	case "Pod":
		return m.Metadata.Annotations, nil

	case "DaemonSet", "Deployment":
		var s workloadSpec
		if err := json.Unmarshal(m.Spec, &s); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
		return s.Template.Metadata.Annotations, nil

	case "Job":
		var s jobSpec
		if err := json.Unmarshal(m.Spec, &s); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
		return s.Template.Metadata.Annotations, nil

	case "CronJob":
		var s cronJobSpec
		if err := json.Unmarshal(m.Spec, &s); err != nil {
			return nil, fmt.Errorf("unmarshal spec: %w", err)
		}
		return s.JobTemplate.Spec.Template.Metadata.Annotations, nil

	default:
		return nil, fmt.Errorf("unsupported kind %q", m.Kind)
	}
}

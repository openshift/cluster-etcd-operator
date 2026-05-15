package tools

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHashMAC(t *testing.T) {
	tests := []struct {
		name    string
		mac     string
		wantErr bool
	}{
		{name: "colon-separated lowercase", mac: "aa:bb:cc:dd:ee:ff"},
		{name: "colon-separated uppercase", mac: "AA:BB:CC:DD:EE:FF"},
		{name: "dash-separated uppercase", mac: "AA-BB-CC-DD-EE-FF"},
		{name: "dash-separated lowercase", mac: "aa-bb-cc-dd-ee-ff"},
		{name: "mixed case", mac: "Aa:Bb:Cc:Dd:Ee:Ff"},
		{name: "invalid MAC", mac: "not-a-mac", wantErr: true},
		{name: "empty string", mac: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HashMAC(tt.mac)
			if (err != nil) != tt.wantErr {
				t.Errorf("HashMAC(%q) error = %v, wantErr %v", tt.mac, err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got) != 64 {
				t.Errorf("HashMAC(%q) = %q, expected 64 hex characters", tt.mac, got)
			}
		})
	}
}

func TestHashMACEquivalentFormats(t *testing.T) {
	formats := []string{
		"AA:BB:CC:DD:EE:FF",
		"aa:bb:cc:dd:ee:ff",
		"AA-BB-CC-DD-EE-FF",
		"aa-bb-cc-dd-ee-ff",
		"Aa:Bb:Cc:Dd:Ee:Ff",
	}

	var first string
	for _, mac := range formats {
		hash, err := HashMAC(mac)
		if err != nil {
			t.Fatalf("HashMAC(%q) unexpected error: %v", mac, err)
		}
		if first == "" {
			first = hash
		} else if hash != first {
			t.Errorf("HashMAC(%q) = %q, want %q (must match all equivalent formats)", mac, hash, first)
		}
	}
}

func newFakeDynClient(machines []map[string]interface{}, bmhs []map[string]interface{}) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	var objects []runtime.Object

	for _, m := range machines {
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "machine.openshift.io/v1beta1",
				"kind":       "Machine",
				"metadata": map[string]interface{}{
					"name":      m["name"],
					"namespace": machineAPINamespace,
				},
				"status": m["status"],
			},
		}
		objects = append(objects, obj)
	}

	for _, b := range bmhs {
		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "metal3.io/v1alpha1",
				"kind":       "BareMetalHost",
				"metadata": map[string]interface{}{
					"name":      b["name"],
					"namespace": machineAPINamespace,
				},
				"spec": b["spec"],
			},
		}
		if s, ok := b["status"]; ok {
			obj.Object["status"] = s
		}
		objects = append(objects, obj)
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			machineGVR: "MachineList",
			bmhGVR:     "BareMetalHostList",
		},
		objects...)
}

func newNodeWithAnnotation(name, machineName string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"machine.openshift.io/machine": machineAPINamespace + "/" + machineName,
			},
		},
	}
}

func newNodeWithProviderID(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func TestGetNodeMACAddresses(t *testing.T) {
	tests := []struct {
		name       string
		nodeName   string
		nodes      []*corev1.Node
		machines   []map[string]interface{}
		bmhs       []map[string]interface{}
		wantMACs   []string
		wantErr    bool
		errContain string
	}{
		{
			name:     "boot MAC only, no hardware status",
			nodeName: "master-0",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-0"},
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01"},
		},
		{
			name:     "boot MAC plus NIC MACs",
			nodeName: "master-0",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-0"},
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:02", "name": "eth0"},
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:03", "name": "eth1"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02", "AA:BB:CC:DD:EE:03"},
		},
		{
			name:     "boot MAC duplicated in NICs is deduplicated",
			nodeName: "master-0",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-0"},
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "aa:bb:cc:dd:ee:01", "name": "eth0"},
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:02", "name": "eth1"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"},
		},
		{
			name:     "no boot MAC, NICs only",
			nodeName: "master-0",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef": map[string]interface{}{"name": "machine-0"},
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:02", "name": "eth0"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:02"},
		},
		{
			name:     "no MACs at all",
			nodeName: "master-0",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef": map[string]interface{}{"name": "machine-0"},
					},
				},
			},
			wantErr:    true,
			errContain: "have no MAC addresses",
		},
		{
			name:     "all fallbacks fail",
			nodeName: "master-99",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			wantErr:    true,
			errContain: "tried Machine CRs, node annotation, providerID",
		},
		{
			name:     "no matching BMH",
			nodeName: "master-0",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
			},
			bmhs:       []map[string]interface{}{},
			wantErr:    true,
			errContain: "no baremetalhosts found",
		},
		{
			name:     "two nodes picks correct BMH",
			nodeName: "master-1",
			machines: []map[string]interface{}{
				{
					"name": "machine-0",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-0"},
					},
				},
				{
					"name": "machine-1",
					"status": map[string]interface{}{
						"nodeRef": map[string]interface{}{"name": "master-1"},
					},
				},
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-0"},
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
				},
				{
					"name": "bmh-1",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-1"},
						"bootMACAddress": "AA:BB:CC:DD:EE:02",
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:03", "name": "eth0"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:02", "AA:BB:CC:DD:EE:03"},
		},
		{
			name:     "annotation fallback when no Machine CRs",
			nodeName: "master-0",
			nodes: []*corev1.Node{
				newNodeWithAnnotation("master-0", "machine-0"),
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-0"},
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01"},
		},
		{
			name:     "annotation fallback with NIC MACs",
			nodeName: "master-0",
			nodes: []*corev1.Node{
				newNodeWithAnnotation("master-0", "machine-0"),
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"consumerRef":    map[string]interface{}{"name": "machine-0"},
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:02", "name": "eth0"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"},
		},
		{
			name:     "providerID fallback when no Machine CRs and no annotation",
			nodeName: "master-0",
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-0", "baremetalhost:///openshift-machine-api/bmh-0/some-uid"),
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:02", "name": "eth0"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01", "AA:BB:CC:DD:EE:02"},
		},
		{
			name:     "providerID fallback with metal3:// scheme",
			nodeName: "master-0",
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-0", "metal3://openshift-machine-api/bmh-0/some-uid"),
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:01"},
		},
		{
			name:     "providerID fallback picks correct BMH",
			nodeName: "master-1",
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-1", "baremetalhost:///openshift-machine-api/bmh-1/uid-1"),
			},
			bmhs: []map[string]interface{}{
				{
					"name": "bmh-0",
					"spec": map[string]interface{}{
						"bootMACAddress": "AA:BB:CC:DD:EE:01",
					},
				},
				{
					"name": "bmh-1",
					"spec": map[string]interface{}{
						"bootMACAddress": "AA:BB:CC:DD:EE:03",
					},
					"status": map[string]interface{}{
						"hardware": map[string]interface{}{
							"nics": []interface{}{
								map[string]interface{}{"mac": "AA:BB:CC:DD:EE:04", "name": "eth0"},
							},
						},
					},
				},
			},
			wantMACs: []string{"AA:BB:CC:DD:EE:03", "AA:BB:CC:DD:EE:04"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var kubeObjects []runtime.Object
			for _, n := range tt.nodes {
				kubeObjects = append(kubeObjects, n)
			}
			kubeClient := fake.NewSimpleClientset(kubeObjects...)
			dynClient := newFakeDynClient(tt.machines, tt.bmhs)

			macs, err := GetNodeMACAddresses(context.Background(), kubeClient, dynClient, tt.nodeName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetNodeMACAddresses() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err != nil && tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("GetNodeMACAddresses() error = %v, want error containing %q", err, tt.errContain)
				}
				return
			}
			if len(macs) != len(tt.wantMACs) {
				t.Errorf("GetNodeMACAddresses() returned %d MACs %v, want %d MACs %v", len(macs), macs, len(tt.wantMACs), tt.wantMACs)
				return
			}
			for i, mac := range macs {
				if mac != tt.wantMACs[i] {
					t.Errorf("GetNodeMACAddresses()[%d] = %q, want %q", i, mac, tt.wantMACs[i])
				}
			}
		})
	}
}

func TestParseBMHProviderID(t *testing.T) {
	tests := []struct {
		providerID    string
		wantName      string
		wantNamespace string
	}{
		{"baremetalhost:///openshift-machine-api/bmh-0/some-uid", "bmh-0", "openshift-machine-api"},
		{"metal3://openshift-machine-api/bmh-0/some-uid", "bmh-0", "openshift-machine-api"},
		{"baremetalhost:///custom-ns/my-host/uid-123", "my-host", "custom-ns"},
		{"aws:///us-east-1/i-123", "", ""},
		{"", "", ""},
		{"metal3://", "", ""},
		{"baremetalhost:///", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.providerID, func(t *testing.T) {
			name, ns := parseBMHProviderID(tt.providerID)
			if name != tt.wantName {
				t.Errorf("parseBMHProviderID(%q) name = %q, want %q", tt.providerID, name, tt.wantName)
			}
			if ns != tt.wantNamespace {
				t.Errorf("parseBMHProviderID(%q) namespace = %q, want %q", tt.providerID, ns, tt.wantNamespace)
			}
		})
	}
}

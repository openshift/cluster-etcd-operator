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

func newFencingSecret(name, address string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: fencingNamespace,
		},
		Data: map[string][]byte{
			"address":                 []byte(address),
			"username":                []byte("admin"),
			"password":                []byte("password"),
			"certificateVerification": []byte("Disabled"),
		},
	}
}

func newNodeWithProviderID(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func newBMH(name, bmcAddress string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "metal3.io/v1alpha1",
			"kind":       "BareMetalHost",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": machineAPINamespace,
			},
			"spec": map[string]interface{}{
				"bmc": map[string]interface{}{
					"address": bmcAddress,
				},
			},
		},
	}
}

func newFakeDynClient(bmhs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	var objects []runtime.Object
	for _, b := range bmhs {
		objects = append(objects, b)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			bmhGVR: "BareMetalHostList",
		},
		objects...)
}

func TestGetFencingSecrets(t *testing.T) {
	const (
		bmcAddr0 = "redfish+https://192.168.111.1:8000/redfish/v1/Systems/uuid-0"
		bmcAddr1 = "redfish+https://192.168.111.1:8000/redfish/v1/Systems/uuid-1"
	)

	tests := []struct {
		name        string
		nodeNames   []string
		nodes       []*corev1.Node
		secrets     []*corev1.Secret
		bmhs        []*unstructured.Unstructured
		wantMap     map[string]string // nodeName → expected secret name
		wantErr     bool
		errContain  string
	}{
		{
			name:      "both nodes found by hostname",
			nodeNames: []string{"master-0", "master-1"},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0", bmcAddr0),
				newFencingSecret("fencing-credentials-master-1", bmcAddr1),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
				"master-1": "fencing-credentials-master-1",
			},
		},
		{
			name:      "one by hostname, one by BMC address",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-1", "baremetalhost:///openshift-machine-api/bmh-1/uid-1"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0", bmcAddr0),
				newFencingSecret("fencing-credentials-somehash", bmcAddr1),
			},
			bmhs: []*unstructured.Unstructured{
				newBMH("bmh-1", bmcAddr1),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
				"master-1": "fencing-credentials-somehash",
			},
		},
		{
			name:      "both by BMC address match",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-0", "baremetalhost:///openshift-machine-api/bmh-0/uid-0"),
				newNodeWithProviderID("master-1", "baremetalhost:///openshift-machine-api/bmh-1/uid-1"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-hash0", bmcAddr0),
				newFencingSecret("fencing-credentials-hash1", bmcAddr1),
			},
			bmhs: []*unstructured.Unstructured{
				newBMH("bmh-0", bmcAddr0),
				newBMH("bmh-1", bmcAddr1),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-hash0",
				"master-1": "fencing-credentials-hash1",
			},
		},
		{
			name:      "one by BMC match, one by elimination",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-0", "baremetalhost:///openshift-machine-api/bmh-0/uid-0"),
				{ObjectMeta: metav1.ObjectMeta{Name: "master-1"}},
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-hash0", bmcAddr0),
				newFencingSecret("fencing-credentials-hash1", bmcAddr1),
			},
			bmhs: []*unstructured.Unstructured{
				newBMH("bmh-0", bmcAddr0),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-hash0",
				"master-1": "fencing-credentials-hash1",
			},
		},
		{
			name:      "no matching secret",
			nodeNames: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-0", "baremetalhost:///openshift-machine-api/bmh-0/uid-0"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-hash0", "redfish+https://other-addr/Systems/other"),
			},
			bmhs: []*unstructured.Unstructured{
				newBMH("bmh-0", bmcAddr0),
			},
			wantErr:    true,
			errContain: "no fencing secret matched BMC address",
		},
		{
			name:      "two unresolved nodes but only one secret left",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "master-1"}},
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-hash0", bmcAddr0),
			},
			wantErr:    true,
			errContain: "cannot resolve fencing secrets",
		},
		{
			name:      "metal3 providerID scheme",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithProviderID("master-0", "metal3://openshift-machine-api/bmh-0/uid-0"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-1", bmcAddr1),
				newFencingSecret("fencing-credentials-hash0", bmcAddr0),
			},
			bmhs: []*unstructured.Unstructured{
				newBMH("bmh-0", bmcAddr0),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-hash0",
				"master-1": "fencing-credentials-master-1",
			},
		},
		{
			name:      "empty node names are skipped",
			nodeNames: []string{"master-0", ""},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0", bmcAddr0),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var kubeObjects []runtime.Object
			for _, s := range tt.secrets {
				kubeObjects = append(kubeObjects, s)
			}
			for _, n := range tt.nodes {
				kubeObjects = append(kubeObjects, n)
			}
			kubeClient := fake.NewSimpleClientset(kubeObjects...)
			dynClient := newFakeDynClient(tt.bmhs...)

			result, err := GetFencingSecrets(context.Background(), kubeClient, dynClient, tt.nodeNames)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetFencingSecrets() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if err != nil && tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("GetFencingSecrets() error = %v, want error containing %q", err, tt.errContain)
				}
				return
			}
			for nodeName, wantSecretName := range tt.wantMap {
				got, ok := result[nodeName]
				if !ok {
					t.Errorf("GetFencingSecrets() missing result for node %s", nodeName)
					continue
				}
				if got.Name != wantSecretName {
					t.Errorf("GetFencingSecrets()[%s] = %q, want %q", nodeName, got.Name, wantSecretName)
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

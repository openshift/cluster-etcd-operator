package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"
)

func newFencingSecret(name string) *corev1.Secret {
	return newFencingSecretWithAddress(name, "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1")
}

func newFencingSecretWithAddress(name, address string) *corev1.Secret {
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

func newNodeWithSystemUUID(name, uuid string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				SystemUUID: uuid,
			},
		},
	}
}

func newNodeWithMACs(name, macs string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				MACAnnotationKey: macs,
			},
		},
	}
}

func newNodeWithMACsAndUUID(name, macs, uuid string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				MACAnnotationKey: macs,
			},
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				SystemUUID: uuid,
			},
		},
	}
}

func TestGetFencingSecrets(t *testing.T) {
	const (
		uuid0    = "a944436f-3d48-4139-8f3c-b0d397947966"
		uuid1    = "b055547g-4d59-5240-9g4d-c1e408058077"
		macHash0 = "261dddd03aae841c2149ad8897e00961b9d429edc07213cbcfd840802d53e43b"
		macHash1 = "73b6d39a7211d7573527aed4a201b5fa7e8801249a7e316ab217fa7532a0cdb5"
	)

	tests := []struct {
		name       string
		nodeNames  []string
		nodes      []*corev1.Node
		secrets    []*corev1.Secret
		mockGetter redfishUUIDGetter
		wantMap    map[string]string
		wantErr    bool
		errContain string
	}{
		{
			name:      "both nodes found by hostname",
			nodeNames: []string{"master-0", "master-1"},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0"),
				newFencingSecret("fencing-credentials-master-1"),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
				"master-1": "fencing-credentials-master-1",
			},
		},
		{
			name:      "both nodes matched via Redfish UUID",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", uuid0),
				newNodeWithSystemUUID("master-1", uuid1),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
				newFencingSecretWithAddress("fencing-credentials-secret-1", "redfish+https://192.168.1.2:8000/redfish/v1/Systems/2"),
			},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				if strings.Contains(address, "192.168.1.1") {
					return uuid0, nil
				}
				return uuid1, nil
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-secret-0",
				"master-1": "fencing-credentials-secret-1",
			},
		},
		{
			name:      "one by hostname, one by Redfish UUID",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-1", uuid1),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0"),
				newFencingSecretWithAddress("fencing-credentials-secret-1", "redfish+https://192.168.1.2:8000/redfish/v1/Systems/2"),
			},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return uuid1, nil
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
				"master-1": "fencing-credentials-secret-1",
			},
		},
		{
			name:      "both nodes matched via MAC hash annotation",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithMACs("master-0", "aa:bb:cc:dd:ee:01"),
				newNodeWithMACs("master-1", "aa:bb:cc:dd:ee:02"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-" + macHash0),
				newFencingSecret("fencing-credentials-" + macHash1),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-" + macHash0,
				"master-1": "fencing-credentials-" + macHash1,
			},
		},
		{
			name:      "one by hostname, one by MAC hash",
			nodeNames: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithMACs("master-1", "aa:bb:cc:dd:ee:02"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0"),
				newFencingSecret("fencing-credentials-" + macHash1),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
				"master-1": "fencing-credentials-" + macHash1,
			},
		},
		{
			name:      "MAC hash fallback to Redfish UUID",
			nodeNames: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithMACsAndUUID("master-0", "ff:ff:ff:ff:ff:ff", uuid0),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return uuid0, nil
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-secret-0",
			},
		},
		{
			name:      "node without MAC annotation falls through to Redfish",
			nodeNames: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", uuid0),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return uuid0, nil
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-secret-0",
			},
		},
		{
			name:      "no matching secret",
			nodeNames: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", uuid0),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-unrelated", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return "different-uuid", nil
			},
			wantErr:    true,
			errContain: "no fencing secret found for node",
		},
		{
			name:      "empty node names are skipped",
			nodeNames: []string{"master-0", ""},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0"),
			},
			wantMap: map[string]string{
				"master-0": "fencing-credentials-master-0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Override the default Redfish getter
			origGetter := defaultRedfishUUIDGetter
			if tt.mockGetter != nil {
				defaultRedfishUUIDGetter = tt.mockGetter
			} else {
				defaultRedfishUUIDGetter = func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
					return "", fmt.Errorf("no Redfish endpoint available in test")
				}
			}
			t.Cleanup(func() { defaultRedfishUUIDGetter = origGetter })

			var kubeObjects []runtime.Object
			for _, s := range tt.secrets {
				kubeObjects = append(kubeObjects, s)
			}
			for _, n := range tt.nodes {
				kubeObjects = append(kubeObjects, n)
			}
			kubeClient := fake.NewSimpleClientset(kubeObjects...)

			result, err := GetFencingSecrets(context.Background(), kubeClient, tt.nodeNames)
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

func TestMatchMACHashToSecrets(t *testing.T) {
	const (
		macHash0 = "261dddd03aae841c2149ad8897e00961b9d429edc07213cbcfd840802d53e43b"
		macHash1 = "73b6d39a7211d7573527aed4a201b5fa7e8801249a7e316ab217fa7532a0cdb5"
	)

	tests := []struct {
		name            string
		unresolvedNodes []string
		nodes           []*corev1.Node
		secrets         []*corev1.Secret
		wantMatched     map[string]string
	}{
		{
			name:            "both nodes matched by MAC hash",
			unresolvedNodes: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithMACs("master-0", "aa:bb:cc:dd:ee:01"),
				newNodeWithMACs("master-1", "aa:bb:cc:dd:ee:02"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-" + macHash0),
				newFencingSecret("fencing-credentials-" + macHash1),
			},
			wantMatched: map[string]string{
				"master-0": "fencing-credentials-" + macHash0,
				"master-1": "fencing-credentials-" + macHash1,
			},
		},
		{
			name:            "node with multiple MACs matches on second",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithMACs("master-0", "ff:ff:ff:ff:ff:ff,aa:bb:cc:dd:ee:01"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-" + macHash0),
			},
			wantMatched: map[string]string{
				"master-0": "fencing-credentials-" + macHash0,
			},
		},
		{
			name:            "no MAC annotation skips node",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "master-0"}},
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-" + macHash0),
			},
			wantMatched: map[string]string{},
		},
		{
			name:            "no matching secret for any MAC",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithMACs("master-0", "ff:ff:ff:ff:ff:01"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-" + macHash0),
			},
			wantMatched: map[string]string{},
		},
		{
			name:            "already resolved node is skipped",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithMACs("master-0", "aa:bb:cc:dd:ee:01"),
			},
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-" + macHash0),
			},
			wantMatched: map[string]string{
				"master-0": "fencing-credentials-preexisting",
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

			result := make(map[string]*corev1.Secret)
			if tt.name == "already resolved node is skipped" {
				result["master-0"] = &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "fencing-credentials-preexisting"},
				}
			}

			err := matchMACHashToSecrets(context.Background(), kubeClient, tt.unresolvedNodes, result)
			if err != nil {
				t.Fatalf("matchMACHashToSecrets() unexpected error: %v", err)
			}

			if len(result) != len(tt.wantMatched) {
				t.Fatalf("matchMACHashToSecrets() matched %d nodes, want %d", len(result), len(tt.wantMatched))
			}
			for nodeName, wantSecretName := range tt.wantMatched {
				got, ok := result[nodeName]
				if !ok {
					t.Errorf("matchMACHashToSecrets() missing result for node %s", nodeName)
					continue
				}
				if got.Name != wantSecretName {
					t.Errorf("matchMACHashToSecrets()[%s] = %q, want %q", nodeName, got.Name, wantSecretName)
				}
			}
		})
	}
}

func TestMatchRedfishUUIDsToSecrets(t *testing.T) {
	const (
		uuid0 = "a944436f-3d48-4139-8f3c-b0d397947966"
		uuid1 = "b055547g-4d59-5240-9g4d-c1e408058077"
	)

	tests := []struct {
		name            string
		unresolvedNodes []string
		nodes           []*corev1.Node
		secrets         []*corev1.Secret
		alreadyAssigned map[string]bool
		mockGetter      redfishUUIDGetter
		wantMatched     map[string]string
	}{
		{
			name:            "both nodes matched by UUID",
			unresolvedNodes: []string{"master-0", "master-1"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", uuid0),
				newNodeWithSystemUUID("master-1", uuid1),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
				newFencingSecretWithAddress("fencing-credentials-secret-1", "redfish+https://192.168.1.2:8000/redfish/v1/Systems/2"),
			},
			alreadyAssigned: map[string]bool{},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				if strings.Contains(address, "192.168.1.1") {
					return uuid0, nil
				}
				return uuid1, nil
			},
			wantMatched: map[string]string{
				"master-0": "fencing-credentials-secret-0",
				"master-1": "fencing-credentials-secret-1",
			},
		},
		{
			name:            "case-insensitive UUID matching",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", "A944436F-3D48-4139-8F3C-B0D397947966"),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			alreadyAssigned: map[string]bool{},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return "a944436f-3d48-4139-8f3c-b0d397947966", nil
			},
			wantMatched: map[string]string{
				"master-0": "fencing-credentials-secret-0",
			},
		},
		{
			name:            "redfish unreachable skips secret",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", uuid0),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			alreadyAssigned: map[string]bool{},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return "", fmt.Errorf("connection refused")
			},
			wantMatched: map[string]string{},
		},
		{
			name:            "already assigned secrets are excluded",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", uuid0),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			alreadyAssigned: map[string]bool{"fencing-credentials-secret-0": true},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return uuid0, nil
			},
			wantMatched: map[string]string{},
		},
		{
			name:            "node with empty systemUUID is skipped",
			unresolvedNodes: []string{"master-0"},
			nodes: []*corev1.Node{
				newNodeWithSystemUUID("master-0", ""),
			},
			secrets: []*corev1.Secret{
				newFencingSecretWithAddress("fencing-credentials-secret-0", "redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			},
			alreadyAssigned: map[string]bool{},
			mockGetter: func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
				return uuid0, nil
			},
			wantMatched: map[string]string{},
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
			result := make(map[string]*corev1.Secret)

			err := matchRedfishUUIDsToSecrets(context.Background(), kubeClient, tt.unresolvedNodes, result, tt.alreadyAssigned, tt.mockGetter)
			if err != nil {
				t.Fatalf("matchRedfishUUIDsToSecrets() unexpected error: %v", err)
			}

			if len(result) != len(tt.wantMatched) {
				t.Fatalf("matchRedfishUUIDsToSecrets() matched %d nodes, want %d", len(result), len(tt.wantMatched))
			}
			for nodeName, wantSecretName := range tt.wantMatched {
				got, ok := result[nodeName]
				if !ok {
					t.Errorf("matchRedfishUUIDsToSecrets() missing result for node %s", nodeName)
					continue
				}
				if got.Name != wantSecretName {
					t.Errorf("matchRedfishUUIDsToSecrets()[%s] = %q, want %q", nodeName, got.Name, wantSecretName)
				}
			}
		})
	}
}

func TestGetFencingSecrets_Phase1APIError(t *testing.T) {
	origGetter := defaultRedfishUUIDGetter
	defaultRedfishUUIDGetter = func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
		return "", fmt.Errorf("should not be called")
	}
	t.Cleanup(func() { defaultRedfishUUIDGetter = origGetter })

	kubeClient := fake.NewSimpleClientset()
	kubeClient.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(
			schema.GroupResource{Resource: "secrets"}, "fencing-credentials-master-0", fmt.Errorf("access denied"))
	})

	_, err := GetFencingSecrets(context.Background(), kubeClient, []string{"master-0"})
	if err == nil {
		t.Fatal("expected error from Phase 1 API failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get secret") {
		t.Errorf("expected 'failed to get secret' error, got: %v", err)
	}
}

func TestGetFencingSecrets_Phase2APIError(t *testing.T) {
	const macHash0 = "261dddd03aae841c2149ad8897e00961b9d429edc07213cbcfd840802d53e43b"

	origGetter := defaultRedfishUUIDGetter
	defaultRedfishUUIDGetter = func(ctx context.Context, address, username, password string, insecure bool) (string, error) {
		return "", fmt.Errorf("should not be called")
	}
	t.Cleanup(func() { defaultRedfishUUIDGetter = origGetter })

	node := newNodeWithMACs("master-0", "aa:bb:cc:dd:ee:01")
	kubeClient := fake.NewSimpleClientset(node)
	kubeClient.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		getAction := action.(k8stesting.GetAction)
		if getAction.GetName() == "fencing-credentials-master-0" {
			return true, nil, errors.NewNotFound(schema.GroupResource{Resource: "secrets"}, getAction.GetName())
		}
		if getAction.GetName() == "fencing-credentials-"+macHash0 {
			return true, nil, errors.NewForbidden(
				schema.GroupResource{Resource: "secrets"}, getAction.GetName(), fmt.Errorf("access denied"))
		}
		return false, nil, nil
	})

	_, err := GetFencingSecrets(context.Background(), kubeClient, []string{"master-0"})
	if err == nil {
		t.Fatal("expected error from Phase 2 API failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get secret") {
		t.Errorf("expected 'failed to get secret' error, got: %v", err)
	}
}

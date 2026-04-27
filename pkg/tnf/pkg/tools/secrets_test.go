package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func mustHashMAC(t *testing.T, mac string) string {
	t.Helper()
	h, err := HashMAC(mac)
	if err != nil {
		t.Fatalf("HashMAC(%q) unexpected error: %v", mac, err)
	}
	return h
}

func newFencingSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "openshift-etcd",
		},
		Data: map[string][]byte{
			"address":                 []byte("redfish+https://192.168.1.1:8000/redfish/v1/Systems/1"),
			"username":                []byte("admin"),
			"password":                []byte("password"),
			"certificateVerification": []byte("Disabled"),
		},
	}
}

func TestGetFencingSecret(t *testing.T) {
	bootMAC := "AA:BB:CC:DD:EE:01"
	nicMAC := "AA:BB:CC:DD:EE:02"

	baseMachine := []map[string]interface{}{
		{
			"name": "machine-0",
			"status": map[string]interface{}{
				"nodeRef": map[string]interface{}{"name": "master-0"},
			},
		},
	}

	bmhWithBothMACs := []map[string]interface{}{
		{
			"name": "bmh-0",
			"spec": map[string]interface{}{
				"consumerRef":    map[string]interface{}{"name": "machine-0"},
				"bootMACAddress": bootMAC,
			},
			"status": map[string]interface{}{
				"hardware": map[string]interface{}{
					"nics": []interface{}{
						map[string]interface{}{"mac": nicMAC, "name": "eth1"},
					},
				},
			},
		},
	}

	tests := []struct {
		name       string
		nodeName   string
		secrets    []*corev1.Secret
		machines   []map[string]interface{}
		bmhs       []map[string]interface{}
		wantSecret string
		wantErr    bool
		errContain string
	}{
		{
			name:     "found by node name",
			nodeName: "master-0",
			secrets: []*corev1.Secret{
				newFencingSecret("fencing-credentials-master-0"),
			},
			wantSecret: "fencing-credentials-master-0",
		},
		{
			name:     "found by boot MAC hash",
			nodeName: "master-0",
			secrets: []*corev1.Secret{
				newFencingSecret(fmt.Sprintf("fencing-credentials-%s", mustHashMAC(t, bootMAC))),
			},
			machines:   baseMachine,
			bmhs:       bmhWithBothMACs,
			wantSecret: fmt.Sprintf("fencing-credentials-%s", mustHashMAC(t, bootMAC)),
		},
		{
			name:     "found by NIC MAC hash, not boot MAC",
			nodeName: "master-0",
			secrets: []*corev1.Secret{
				newFencingSecret(fmt.Sprintf("fencing-credentials-%s", mustHashMAC(t, nicMAC))),
			},
			machines:   baseMachine,
			bmhs:       bmhWithBothMACs,
			wantSecret: fmt.Sprintf("fencing-credentials-%s", mustHashMAC(t, nicMAC)),
		},
		{
			name:     "no secret found for any MAC",
			nodeName: "master-0",
			machines: baseMachine,
			bmhs:     bmhWithBothMACs,
			wantErr:  true,
			errContain: "no fencing secret found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var kubeObjects []runtime.Object
			for _, s := range tt.secrets {
				kubeObjects = append(kubeObjects, s)
			}
			kubeClient := fake.NewSimpleClientset(kubeObjects...)
			dynClient := newFakeDynClient(tt.machines, tt.bmhs)

			secret, err := GetFencingSecret(context.Background(), kubeClient, dynClient, tt.nodeName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetFencingSecret() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err != nil && tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("GetFencingSecret() error = %v, want error containing %q", err, tt.errContain)
				}
				return
			}
			if secret == nil {
				t.Fatal("GetFencingSecret() returned nil secret without error")
			}
			if secret.Name != tt.wantSecret {
				t.Errorf("GetFencingSecret() returned secret %q, want %q", secret.Name, tt.wantSecret)
			}
		})
	}
}

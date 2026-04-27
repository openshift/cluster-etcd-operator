package tools

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHashMAC(t *testing.T) {
	tests := []struct {
		name    string
		mac     string
		wantErr bool
	}{
		{
			name: "standard colon-separated MAC",
			mac:  "aa:bb:cc:dd:ee:ff",
		},
		{
			name: "uppercase MAC produces same hash as lowercase",
			mac:  "AA:BB:CC:DD:EE:FF",
		},
		{
			name: "dash-separated MAC",
			mac:  "aa-bb-cc-dd-ee-ff",
		},
		{
			name:    "invalid MAC",
			mac:     "not-a-mac",
			wantErr: true,
		},
		{
			name:    "empty string",
			mac:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := hashMAC(tt.mac)
			if (err != nil) != tt.wantErr {
				t.Fatalf("hashMAC(%q) error = %v, wantErr %v", tt.mac, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if hash == "" {
				t.Fatal("hashMAC() returned empty hash")
			}
			if len(hash) != 64 {
				t.Errorf("hashMAC() hash length = %d, want 64 (SHA256 hex)", len(hash))
			}
		})
	}

	hash1, _ := hashMAC("aa:bb:cc:dd:ee:ff")
	hash2, _ := hashMAC("AA:BB:CC:DD:EE:FF")
	hash3, _ := hashMAC("aa-bb-cc-dd-ee-ff")
	if hash1 != hash2 {
		t.Errorf("lowercase and uppercase MACs should produce the same hash: %s != %s", hash1, hash2)
	}
	if hash1 != hash3 {
		t.Errorf("colon and dash separated MACs should produce the same hash: %s != %s", hash1, hash3)
	}
}

func TestAnnotateNodeMACs(t *testing.T) {
	tests := []struct {
		name      string
		nodeName  string
		mockMACs  []string
		mockErr   error
		wantErr   bool
		wantAnnot string
	}{
		{
			name:      "annotate existing node with discovered MACs",
			nodeName:  "master-0",
			mockMACs:  []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"},
			wantAnnot: "aa:bb:cc:dd:ee:01,aa:bb:cc:dd:ee:02",
		},
		{
			name:      "single MAC",
			nodeName:  "master-0",
			mockMACs:  []string{"aa:bb:cc:dd:ee:01"},
			wantAnnot: "aa:bb:cc:dd:ee:01",
		},
		{
			name:     "node not found",
			nodeName: "nonexistent-node",
			mockMACs: []string{"aa:bb:cc:dd:ee:01"},
			wantErr:  true,
		},
		{
			name:     "no MACs found",
			nodeName: "master-0",
			mockMACs: nil,
			wantErr:  true,
		},
		{
			name:     "MAC reader error",
			nodeName: "master-0",
			mockErr:  fmt.Errorf("nsenter failed"),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origReader := defaultMACReader
			defaultMACReader = func(ctx context.Context) ([]string, error) {
				if tt.mockErr != nil {
					return nil, tt.mockErr
				}
				return tt.mockMACs, nil
			}
			t.Cleanup(func() { defaultMACReader = origReader })

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "master-0"},
			}
			kubeClient := fake.NewSimpleClientset(node)

			err := AnnotateNodeMACs(context.Background(), kubeClient, tt.nodeName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AnnotateNodeMACs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			updated, err := kubeClient.CoreV1().Nodes().Get(context.Background(), tt.nodeName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to get node: %v", err)
			}
			got := updated.Annotations[MACAnnotationKey]
			if got != tt.wantAnnot {
				t.Errorf("Node annotation = %q, want %q", got, tt.wantAnnot)
			}
		})
	}
}

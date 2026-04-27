package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	MACAnnotationKey = "tnf.openshift.io/mac-addresses"
)

type macReader func(ctx context.Context) ([]string, error)

var defaultMACReader macReader = readHostMACs

func AnnotateNodeMACs(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) error {
	macs, err := defaultMACReader(ctx)
	if err != nil {
		return fmt.Errorf("failed to read host MAC addresses: %w", err)
	}
	if len(macs) == 0 {
		return fmt.Errorf("no MAC addresses found on host")
	}

	annotation := strings.Join(macs, ",")
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				MACAnnotationKey: annotation,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	_, err = kubeClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	klog.Infof("Annotated node %s with MAC addresses: %s", nodeName, annotation)
	return nil
}

// readHostMACs reads all non-loopback MAC addresses from the host
// network interfaces via nsenter.
func readHostMACs(ctx context.Context) ([]string, error) {
	shellCmd := `for iface in /sys/class/net/*; do name=$(basename "$iface"); [ "$name" = "lo" ] && continue; cat "$iface/address" 2>/dev/null; done`
	cmd := exec.CommandContext(ctx, "/usr/bin/nsenter", "-a", "-t", "1", "/bin/bash", "-c", shellCmd)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to read MACs: %w (stderr: %s)", err, stderrBuf.String())
	}
	stdout := stdoutBuf.String()

	seen := make(map[string]bool)
	var macs []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		mac := strings.TrimSpace(line)
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		parsed, err := net.ParseMAC(mac)
		if err != nil {
			continue
		}
		normalized := parsed.String()
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		macs = append(macs, mac)
	}

	return macs, nil
}

// hashMAC normalizes a MAC address and returns its SHA256 hash (hex-encoded).
// This matches the algorithm used by openshift/installer to generate fencing
// secret names from MAC addresses.
func hashMAC(macAddress string) (string, error) {
	parsed, err := net.ParseMAC(macAddress)
	if err != nil {
		return "", fmt.Errorf("invalid MAC address %q: %w", macAddress, err)
	}
	normalized := strings.ReplaceAll(strings.ToLower(parsed.String()), ":", "")
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:]), nil
}

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

var (
	machineGVR = schema.GroupVersionResource{
		Group:    "machine.openshift.io",
		Version:  "v1beta1",
		Resource: "machines",
	}
	bmhGVR = schema.GroupVersionResource{
		Group:    "metal3.io",
		Version:  "v1alpha1",
		Resource: "baremetalhosts",
	}

	machineAPINamespace = "openshift-machine-api"
)

// HashMAC normalizes a MAC address and returns its full SHA256 hash
// (hex-encoded). This matches the algorithm used by openshift/installer
// to generate fencing secret names from MAC addresses.
func HashMAC(macAddress string) (string, error) {
	parsed, err := net.ParseMAC(macAddress)
	if err != nil {
		return "", fmt.Errorf("invalid MAC address %q: %w", macAddress, err)
	}
	normalized := strings.ReplaceAll(strings.ToLower(parsed.String()), ":", "")
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:]), nil
}

// GetNodeMACAddresses resolves all MAC addresses for a node by looking up
// the BareMetalHost CR associated with the node's Machine. Returns
// bootMACAddress first (if present), followed by any additional NICs
// from status.hardware.nics.
//
// When Machine CRs are absent (e.g. Assisted Installer deployments),
// falls back to reading the node's machine.openshift.io/machine annotation.
func GetNodeMACAddresses(ctx context.Context, kubeClient kubernetes.Interface, dyClient dynamic.Interface, nodeName string) ([]string, error) {
	machineName, err := findMachineForNode(ctx, dyClient, nodeName)
	if err != nil {
		klog.Infof("Machine CR lookup failed for node %s: %v; trying node annotation fallback", nodeName, err)
		machineName, err = findMachineNameFromNodeAnnotation(ctx, kubeClient, nodeName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve machine for node %s: %w", nodeName, err)
		}
	}

	macs, err := findBMHMACs(ctx, dyClient, machineName)
	if err != nil {
		return nil, fmt.Errorf("failed to find MACs for machine %s: %w", machineName, err)
	}

	klog.Infof("Resolved %d MAC(s) for node %s (machine %s): %v", len(macs), nodeName, machineName, macs)
	return macs, nil
}

// findMachineNameFromNodeAnnotation reads the machine.openshift.io/machine
// annotation from the Node object to get the machine name when Machine CRs
// are not available.
func findMachineNameFromNodeAnnotation(ctx context.Context, kubeClient kubernetes.Interface, nodeName string) (string, error) {
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	annotation, ok := node.Annotations["machine.openshift.io/machine"]
	if !ok || annotation == "" {
		return "", fmt.Errorf("node %s has no machine.openshift.io/machine annotation", nodeName)
	}

	// Annotation format is "namespace/name"
	parts := strings.SplitN(annotation, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", fmt.Errorf("node %s has malformed machine annotation: %q", nodeName, annotation)
	}

	klog.Infof("Resolved machine name %q for node %s from annotation", parts[1], nodeName)
	return parts[1], nil
}

func findMachineForNode(ctx context.Context, dyClient dynamic.Interface, nodeName string) (string, error) {
	machines, err := dyClient.Resource(machineGVR).Namespace(machineAPINamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to list machines: %w", err)
	}
	for _, m := range machines.Items {
		status, ok := m.Object["status"].(map[string]interface{})
		if !ok {
			continue
		}
		nodeRef, ok := status["nodeRef"].(map[string]interface{})
		if !ok {
			continue
		}
		name, ok := nodeRef["name"].(string)
		if !ok {
			continue
		}
		if name == nodeName {
			return m.GetName(), nil
		}
	}
	return "", fmt.Errorf("no machine found with nodeRef pointing to node %s", nodeName)
}

// findBMHMACs returns all unique MAC addresses from the BareMetalHost
// associated with the given machine. bootMACAddress is returned first,
// followed by any additional MACs from status.hardware.nics.
func findBMHMACs(ctx context.Context, dyClient dynamic.Interface, machineName string) ([]string, error) {
	bmhList, err := dyClient.Resource(bmhGVR).Namespace(machineAPINamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list baremetalhosts: %w", err)
	}

	foundMatch := false
	for _, bmh := range bmhList.Items {
		spec, ok := bmh.Object["spec"].(map[string]interface{})
		if !ok {
			continue
		}
		consumerRef, ok := spec["consumerRef"].(map[string]interface{})
		if !ok {
			continue
		}
		name, ok := consumerRef["name"].(string)
		if !ok {
			continue
		}
		if name != machineName {
			continue
		}
		foundMatch = true

		var macs []string
		seen := make(map[string]bool)

		if bootMAC, ok := spec["bootMACAddress"].(string); ok && bootMAC != "" {
			normalized := normalizeMAC(bootMAC)
			if normalized != "" {
				seen[normalized] = true
				macs = append(macs, bootMAC)
			}
		}

		macs = append(macs, extractNICMACs(bmh.Object, seen)...)

		if len(macs) == 0 {
			continue
		}
		return macs, nil
	}
	if foundMatch {
		return nil, fmt.Errorf("baremetalhost(s) for machine %s have no MAC addresses", machineName)
	}
	return nil, fmt.Errorf("no baremetalhosts found with consumerRef pointing to machine %s", machineName)
}

func extractNICMACs(bmhObject map[string]interface{}, seen map[string]bool) []string {
	status, ok := bmhObject["status"].(map[string]interface{})
	if !ok {
		return nil
	}
	hardware, ok := status["hardware"].(map[string]interface{})
	if !ok {
		return nil
	}
	nics, ok := hardware["nics"].([]interface{})
	if !ok {
		return nil
	}

	var macs []string
	for _, nic := range nics {
		nicMap, ok := nic.(map[string]interface{})
		if !ok {
			continue
		}
		mac, ok := nicMap["mac"].(string)
		if !ok || mac == "" {
			continue
		}
		normalized := normalizeMAC(mac)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		macs = append(macs, mac)
	}
	return macs
}

func normalizeMAC(mac string) string {
	parsed, err := net.ParseMAC(mac)
	if err != nil {
		return ""
	}
	return parsed.String()
}

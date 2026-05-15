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
// Fallback order when the primary lookup fails:
//  1. Machine CR (status.nodeRef.name → machine name → BMH consumerRef)
//  2. Node annotation (machine.openshift.io/machine → machine name → BMH consumerRef)
//  3. Node providerID (baremetalhost:///ns/name/uid → BMH directly)
func GetNodeMACAddresses(ctx context.Context, kubeClient kubernetes.Interface, dyClient dynamic.Interface, nodeName string) ([]string, error) {
	machineName, err := findMachineForNode(ctx, dyClient, nodeName)
	if err != nil {
		klog.Infof("Machine CR lookup failed for node %s: %v; trying node annotation fallback", nodeName, err)
		machineName, err = findMachineNameFromNodeAnnotation(ctx, kubeClient, nodeName)
		if err != nil {
			klog.Infof("Node annotation fallback failed for node %s: %v; trying providerID fallback", nodeName, err)
			macs, pidErr := findBMHMACsByProviderID(ctx, kubeClient, dyClient, nodeName)
			if pidErr != nil {
				return nil, fmt.Errorf("failed to resolve MACs for node %s (tried Machine CRs, node annotation, providerID): %w", nodeName, pidErr)
			}
			klog.Infof("Resolved %d MAC(s) for node %s via providerID: %v", len(macs), nodeName, macs)
			return macs, nil
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

// findBMHMACsByProviderID resolves the node's BareMetalHost via the
// node's spec.providerID field (format: "baremetalhost:///ns/name/uid"
// or "metal3://ns/name/uid") and extracts its MAC addresses.
func findBMHMACsByProviderID(ctx context.Context, kubeClient kubernetes.Interface, dyClient dynamic.Interface, nodeName string) ([]string, error) {
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		return nil, fmt.Errorf("node %s has no providerID set", nodeName)
	}

	bmhName, bmhNamespace := parseBMHProviderID(providerID)
	if bmhName == "" {
		return nil, fmt.Errorf("node %s providerID %q is not a baremetalhost reference", nodeName, providerID)
	}
	if bmhNamespace == "" {
		bmhNamespace = machineAPINamespace
	}

	klog.Infof("Resolved BMH %s/%s from node %s providerID", bmhNamespace, bmhName, nodeName)

	bmh, err := dyClient.Resource(bmhGVR).Namespace(bmhNamespace).Get(ctx, bmhName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get baremetalhost %s/%s: %w", bmhNamespace, bmhName, err)
	}

	var macs []string
	seen := make(map[string]bool)

	if spec, ok := bmh.Object["spec"].(map[string]interface{}); ok {
		if bootMAC, ok := spec["bootMACAddress"].(string); ok && bootMAC != "" {
			normalized := normalizeMAC(bootMAC)
			if normalized != "" {
				seen[normalized] = true
				macs = append(macs, bootMAC)
			}
		}
	}

	macs = append(macs, extractNICMACs(bmh.Object, seen)...)

	if len(macs) == 0 {
		return nil, fmt.Errorf("baremetalhost %s/%s has no MAC addresses", bmhNamespace, bmhName)
	}
	return macs, nil
}

// parseBMHProviderID extracts the BMH namespace and name from a
// providerID string. Supported formats:
//   - baremetalhost:///namespace/name/uid
//   - metal3://namespace/name/uid
func parseBMHProviderID(providerID string) (name, namespace string) {
	var path string
	switch {
	case strings.HasPrefix(providerID, "baremetalhost:///"):
		path = strings.TrimPrefix(providerID, "baremetalhost:///")
	case strings.HasPrefix(providerID, "metal3://"):
		path = strings.TrimPrefix(providerID, "metal3://")
	default:
		return "", ""
	}

	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	return parts[1], parts[0]
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

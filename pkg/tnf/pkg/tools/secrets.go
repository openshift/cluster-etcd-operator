package tools

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	fencingSecretNamePrefix = "fencing-credentials-"
	fencingNamespace        = "openshift-etcd"
)

var (
	fencingSecretNamePattern = fmt.Sprintf("%s%s", fencingSecretNamePrefix, "%s")

	bmhGVR = schema.GroupVersionResource{
		Group:    "metal3.io",
		Version:  "v1alpha1",
		Resource: "baremetalhosts",
	}

	machineAPINamespace = "openshift-machine-api"
)

// GetFencingSecrets resolves fencing secrets for each node using a multi-phase
// lookup strategy:
//  1. Hostname: try fencing-credentials-{nodeName} directly
//  2. BMC address match: resolve the node's BareMetalHost via providerID,
//     read spec.bmc.address, and match against remaining secrets' data.address
//  3. Elimination: if exactly one node and one secret remain unmatched, pair them
func GetFencingSecrets(ctx context.Context, kubeClient kubernetes.Interface, dyClient dynamic.Interface, nodeNames []string) (map[string]*corev1.Secret, error) {
	result := make(map[string]*corev1.Secret, len(nodeNames))
	unresolved := make([]string, 0)

	// Phase 1: hostname-based lookup
	for _, nodeName := range nodeNames {
		if nodeName == "" {
			continue
		}
		secretName := fmt.Sprintf(fencingSecretNamePattern, nodeName)
		secret, err := kubeClient.CoreV1().Secrets(fencingNamespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			klog.Infof("Found fencing secret %s for node %s by hostname", secretName, nodeName)
			result[nodeName] = secret
			continue
		}
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get secret %s: %w", secretName, err)
		}
		klog.Infof("Fencing secret %s not found for node %s, deferring to BMC address match", secretName, nodeName)
		unresolved = append(unresolved, nodeName)
	}

	if len(unresolved) == 0 {
		return result, nil
	}

	// Phase 2: list all fencing secrets and filter out already-assigned ones
	remaining, err := listUnclaimedFencingSecrets(ctx, kubeClient, result)
	if err != nil {
		return nil, err
	}

	if len(remaining) == 0 {
		return nil, fmt.Errorf("no fencing secrets available for %d unresolved node(s): %v", len(unresolved), unresolved)
	}

	// Phase 3: BMC address match via providerID → BMH → spec.bmc.address
	stillUnresolved := make([]string, 0)
	for _, nodeName := range unresolved {
		bmcAddress, err := getNodeBMCAddress(ctx, kubeClient, dyClient, nodeName)
		if err != nil {
			klog.Infof("Could not resolve BMC address for node %s: %v; deferring to elimination", nodeName, err)
			stillUnresolved = append(stillUnresolved, nodeName)
			continue
		}

		matched := false
		for i, secret := range remaining {
			secretAddress := string(secret.Data["address"])
			if secretAddress == bmcAddress {
				klog.Infof("Matched fencing secret %s to node %s via BMC address %s", secret.Name, nodeName, bmcAddress)
				result[nodeName] = secret
				remaining = append(remaining[:i], remaining[i+1:]...)
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("no fencing secret matched BMC address %s for node %s", bmcAddress, nodeName)
		}
	}

	// Phase 4: elimination — assign remaining secrets to remaining nodes
	if len(stillUnresolved) > 0 {
		if len(stillUnresolved) == 1 && len(remaining) == 1 {
			nodeName := stillUnresolved[0]
			secret := remaining[0]
			klog.Infof("Assigned fencing secret %s to node %s by elimination", secret.Name, nodeName)
			result[nodeName] = secret
			stillUnresolved = nil
		} else if len(stillUnresolved) > 0 {
			return nil, fmt.Errorf(
				"cannot resolve fencing secrets for %d node(s) %v: %d unclaimed secret(s) remaining (need exactly 1:1 for elimination)",
				len(stillUnresolved), stillUnresolved, len(remaining),
			)
		}
	}

	return result, nil
}

func listUnclaimedFencingSecrets(ctx context.Context, kubeClient kubernetes.Interface, claimed map[string]*corev1.Secret) ([]*corev1.Secret, error) {
	secretList, err := kubeClient.CoreV1().Secrets(fencingNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list secrets in %s: %w", fencingNamespace, err)
	}

	claimedNames := make(map[string]bool, len(claimed))
	for _, s := range claimed {
		claimedNames[s.Name] = true
	}

	var remaining []*corev1.Secret
	for i := range secretList.Items {
		s := &secretList.Items[i]
		if !strings.HasPrefix(s.Name, fencingSecretNamePrefix) {
			continue
		}
		if claimedNames[s.Name] {
			continue
		}
		remaining = append(remaining, s)
	}
	return remaining, nil
}

func getNodeBMCAddress(ctx context.Context, kubeClient kubernetes.Interface, dyClient dynamic.Interface, nodeName string) (string, error) {
	node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	providerID := node.Spec.ProviderID
	if providerID == "" {
		return "", fmt.Errorf("node %s has no providerID", nodeName)
	}

	bmhName, bmhNamespace := parseBMHProviderID(providerID)
	if bmhName == "" {
		return "", fmt.Errorf("node %s providerID %q is not a baremetalhost reference", nodeName, providerID)
	}
	if bmhNamespace == "" {
		bmhNamespace = machineAPINamespace
	}

	bmh, err := dyClient.Resource(bmhGVR).Namespace(bmhNamespace).Get(ctx, bmhName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get baremetalhost %s/%s: %w", bmhNamespace, bmhName, err)
	}

	spec, ok := bmh.Object["spec"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("baremetalhost %s/%s has no spec", bmhNamespace, bmhName)
	}
	bmc, ok := spec["bmc"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("baremetalhost %s/%s has no spec.bmc", bmhNamespace, bmhName)
	}
	address, ok := bmc["address"].(string)
	if !ok || address == "" {
		return "", fmt.Errorf("baremetalhost %s/%s has no spec.bmc.address", bmhNamespace, bmhName)
	}

	klog.Infof("Resolved BMC address %s for node %s (BMH %s/%s)", address, nodeName, bmhNamespace, bmhName)
	return address, nil
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

func IsFencingSecret(secretName string) bool {
	return strings.HasPrefix(secretName, fencingSecretNamePrefix)
}

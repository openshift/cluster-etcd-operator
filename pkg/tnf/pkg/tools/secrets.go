package tools

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	fencingSecretNamePrefix = "fencing-credentials-"
	fencingNamespace        = "openshift-etcd"
)

var fencingSecretNamePattern = fmt.Sprintf("%s%s", fencingSecretNamePrefix, "%s")

// GetFencingSecrets resolves fencing secrets for each node:
//  1. Hostname: try fencing-credentials-{nodeName} directly
//  2. MAC hash: read MAC addresses from node annotation, hash each,
//     try fencing-credentials-{hash} by name.
//  3. Redfish UUID: query each unclaimed fencing secret's Redfish endpoint
//     for the system UUID, match against node status.nodeInfo.systemUUID.
func GetFencingSecrets(ctx context.Context, kubeClient kubernetes.Interface, nodeNames []string) (map[string]*corev1.Secret, error) {
	result := make(map[string]*corev1.Secret, len(nodeNames))
	var unresolved []string

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
		klog.Infof("Fencing secret %s not found for node %s, deferring to MAC hash lookup", secretName, nodeName)
		unresolved = append(unresolved, nodeName)
	}

	if len(unresolved) == 0 {
		return result, nil
	}

	// Phase 2: MAC hash from node annotation
	err := matchMACHashToSecrets(ctx, kubeClient, unresolved, result)
	if err != nil {
		return nil, err
	}

	// Recompute unresolved after Phase 2
	var stillUnresolved []string
	for _, nodeName := range unresolved {
		if _, ok := result[nodeName]; !ok {
			stillUnresolved = append(stillUnresolved, nodeName)
		}
	}

	if len(stillUnresolved) == 0 {
		return result, nil
	}

	// Phase 3: Redfish UUID matching
	alreadyAssigned := make(map[string]bool, len(result))
	for _, secret := range result {
		alreadyAssigned[secret.Name] = true
	}
	err = matchRedfishUUIDsToSecrets(ctx, kubeClient, stillUnresolved, result, alreadyAssigned, defaultRedfishUUIDGetter)
	if err != nil {
		return nil, err
	}

	for _, nodeName := range unresolved {
		if _, ok := result[nodeName]; !ok {
			return nil, fmt.Errorf("no fencing secret found for node %s", nodeName)
		}
	}

	return result, nil
}

// matchMACHashToSecrets reads MAC addresses from node annotations,
// hashes each one, and tries to find a matching fencing secret by name.
func matchMACHashToSecrets(ctx context.Context, kubeClient kubernetes.Interface, unresolvedNodes []string, result map[string]*corev1.Secret) error {
	for _, nodeName := range unresolvedNodes {
		if _, ok := result[nodeName]; ok {
			continue
		}

		node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Could not get node %s for MAC hash lookup: %v", nodeName, err)
			continue
		}

		macAnnotation := node.Annotations[MACAnnotationKey]
		if macAnnotation == "" {
			klog.Infof("Node %s has no MAC address annotation, skipping MAC hash lookup", nodeName)
			continue
		}

		macs := strings.Split(macAnnotation, ",")
		for _, mac := range macs {
			mac = strings.TrimSpace(mac)
			if mac == "" {
				continue
			}
			hash, err := hashMAC(mac)
			if err != nil {
				klog.Warningf("Skipping invalid MAC %s on node %s: %v", mac, nodeName, err)
				continue
			}
			secretName := fmt.Sprintf(fencingSecretNamePattern, hash)
			secret, err := kubeClient.CoreV1().Secrets(fencingNamespace).Get(ctx, secretName, metav1.GetOptions{})
			if err == nil {
				klog.Infof("Matched fencing secret %s to node %s via MAC hash (MAC %s)", secretName, nodeName, mac)
				result[nodeName] = secret
				break
			}
			if !errors.IsNotFound(err) {
				return fmt.Errorf("failed to get secret %s: %w", secretName, err)
			}
		}
	}

	return nil
}

// matchRedfishUUIDsToSecrets queries unclaimed fencing secrets' Redfish endpoints
// for the system UUID and matches against node status.nodeInfo.systemUUID.
func matchRedfishUUIDsToSecrets(ctx context.Context, kubeClient kubernetes.Interface, unresolvedNodes []string, result map[string]*corev1.Secret, alreadyAssigned map[string]bool, uuidGetter redfishUUIDGetter) error {
	secretList, err := kubeClient.CoreV1().Secrets(fencingNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list secrets in %s: %w", fencingNamespace, err)
	}

	type candidate struct {
		secret *corev1.Secret
		uuid   string
	}
	var candidates []candidate

	for i := range secretList.Items {
		s := &secretList.Items[i]
		if !IsFencingSecret(s.Name) || alreadyAssigned[s.Name] {
			continue
		}

		address := string(s.Data["address"])
		if address == "" {
			continue
		}
		redfishURL, err := parseRedfishURL(address)
		if err != nil {
			klog.Warningf("Skipping secret %s: %v", s.Name, err)
			continue
		}

		username := string(s.Data["username"])
		password := string(s.Data["password"])
		insecure := string(s.Data["certificateVerification"]) == "Disabled"

		uuid, err := uuidGetter(ctx, redfishURL, username, password, insecure)
		if err != nil {
			klog.Warningf("Could not get Redfish UUID from secret %s: %v", s.Name, err)
			continue
		}

		candidates = append(candidates, candidate{secret: s, uuid: uuid})
	}

	if len(candidates) == 0 {
		return nil
	}

	for _, nodeName := range unresolvedNodes {
		if _, ok := result[nodeName]; ok {
			continue
		}
		node, err := kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Could not get node %s for UUID matching: %v", nodeName, err)
			continue
		}
		nodeUUID := node.Status.NodeInfo.SystemUUID
		if nodeUUID == "" {
			klog.Warningf("Node %s has empty systemUUID, skipping Redfish UUID matching", nodeName)
			continue
		}

		for _, c := range candidates {
			if strings.EqualFold(c.uuid, nodeUUID) {
				klog.Infof("Matched fencing secret %s to node %s via Redfish UUID %s", c.secret.Name, nodeName, c.uuid)
				result[nodeName] = c.secret
				break
			}
		}
	}

	return nil
}

func IsFencingSecret(secretName string) bool {
	return strings.HasPrefix(secretName, fencingSecretNamePrefix)
}

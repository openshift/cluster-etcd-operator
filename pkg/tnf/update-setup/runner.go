package updatesetup

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// pickLatestUpdateSetupConfigMap returns the ConfigMap with the highest generation.
// During normal operation, there should only be one ConfigMap (old ones are cleaned up),
// but this picks the latest to be safe.
func pickLatestUpdateSetupConfigMap(items []corev1.ConfigMap) (*corev1.ConfigMap, error) {
	var best *corev1.ConfigMap
	var bestGen int64 = -1
	for i := range items {
		cm := &items[i]
		genStr := cm.Data["generation"]
		g, err := strconv.ParseInt(genStr, 10, 64)
		if err != nil {
			klog.Warningf("update-setup ConfigMap %s has invalid generation %q: %v", cm.Name, genStr, err)
			continue
		}
		if g > bestGen {
			bestGen = g
			best = cm
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no update-setup ConfigMap found")
	}
	return best, nil
}

func decodeStringList(data string) ([]string, error) {
	if strings.TrimSpace(data) == "" || data == "[]" {
		return []string{}, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(data), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func RunTnfUpdateSetup() error {

	klog.Info("Setting up clients etc. for TNF update-setup")

	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(clock.RealClock{}, clientConfig, operatorv1.GroupVersion.WithResource("etcds"), operatorv1.GroupVersion.WithKind("Etcd"), operator.ExtractStaticPodOperatorSpec, operator.ExtractStaticPodOperatorStatus)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	dynamicInformers.Start(ctx.Done())
	dynamicInformers.WaitForCacheSync(ctx.Done())

	// Get the current node name from environment (used for cluster config and logging only)
	currentNodeName := os.Getenv("MY_NODE_NAME")
	if currentNodeName == "" {
		return fmt.Errorf("MY_NODE_NAME environment variable not set")
	}

	klog.Infof("Running TNF update-setup on node %s", currentNodeName)

	// check if cluster is running on this node
	command := "/usr/sbin/pcs cluster status"
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		klog.Infof("Cluster not running (err: %v), skipping update-setup on this node", err)
		return nil
	}

	// Pick the latest update-setup ConfigMap (cluster-wide, not node-specific)
	cmList, err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=" + tools.TnfUpdateSetupComponentValue,
	})
	if err != nil {
		return fmt.Errorf("failed to list update-setup ConfigMaps: %w", err)
	}

	cm, err := pickLatestUpdateSetupConfigMap(cmList.Items)
	if err != nil {
		return err
	}

	klog.Infof("Using ConfigMap %s (generation: %s, timestamp: %s)",
		cm.Name, cm.Data["generation"], cm.Data["timestamp"])

	capturedNodes, err := decodeNodeList(cm.Data["nodes"])
	if err != nil {
		return fmt.Errorf("failed to decode node list: %w", err)
	}

	klog.Infof("Captured node list: %v", getNodeNames(capturedNodes))

	cfg, err := buildClusterConfigFromNodeList(capturedNodes)
	if err != nil {
		return err
	}

	// Determine which node we are and get node IPs
	var currentNodeIP, otherNodeName, otherNodeIP string
	if cfg.NodeName1 == currentNodeName {
		currentNodeIP = cfg.NodeIP1
		otherNodeName = cfg.NodeName2
		otherNodeIP = cfg.NodeIP2
	} else if cfg.NodeName2 == currentNodeName {
		currentNodeIP = cfg.NodeIP2
		otherNodeName = cfg.NodeName1
		otherNodeIP = cfg.NodeIP1
	} else {
		return fmt.Errorf("current node %s not found in cluster config (nodes: %s, %s)", currentNodeName, cfg.NodeName1, cfg.NodeName2)
	}

	currentNodeIPs := toSet(cfg.NodeIP1, cfg.NodeIP2)

	// Log node configuration based on cluster size
	if len(capturedNodes) == 1 {
		klog.Infof("Running in single-node mode - Current node: %q (IP: %s)", currentNodeName, currentNodeIP)
	} else {
		klog.Infof("Running in two-node mode - Current node: %q (IP: %s), Other node: %q (IP: %s)", currentNodeName, currentNodeIP, otherNodeName, otherNodeIP)
	}

	// Read reconciliation decisions from ConfigMap (pre-calculated by lifecycle_manager)
	nodesToRemove, err := decodeStringList(cm.Data["nodesToRemove"])
	if err != nil {
		return fmt.Errorf("failed to decode nodesToRemove: %w", err)
	}

	nodesToAdd, err := decodeStringList(cm.Data["nodesToAdd"])
	if err != nil {
		return fmt.Errorf("failed to decode nodesToAdd: %w", err)
	}

	if len(nodesToRemove) == 0 && len(nodesToAdd) == 0 {
		klog.Info("No membership changes needed (nodesToAdd and nodesToRemove are empty)")
	}
	if len(nodesToRemove) > 0 {
		klog.Infof("Will remove: %v", nodesToRemove)
	}
	if len(nodesToAdd) > 0 {
		klog.Infof("Will add: %v", nodesToAdd)
	}

	// Remove nodes from pacemaker if needed
	if len(nodesToRemove) > 0 {
		for _, nodeToRemove := range nodesToRemove {
			if err := removePacemakerNodeGlobally(ctx, nodeToRemove); err != nil {
				return err
			}
		}

		// Remove unstarted etcd members (from deleted nodes)
		if err := removeUnstartedEtcdMembers(ctx, currentNodeIPs); err != nil {
			return err
		}
	}

	// Add nodes to pacemaker if needed
	if len(nodesToAdd) > 0 {
		// Wait for etcd revision stability before adding nodes to pacemaker
		// (ensures new node has etcd manifests before cluster start)
		klog.Info("Waiting for etcd revision update before adding nodes...")
		if err = etcd.WaitForStableRevision(ctx, operatorClient); err != nil {
			return fmt.Errorf("failed to wait for etcd revision stability: %w", err)
		}

		var commands []string
		for _, nodeToAdd := range nodesToAdd {
			commands = append(commands, fmt.Sprintf("/usr/sbin/pcs cluster node add %s", nodeToAdd))
		}
		if err = runCommands(ctx, commands); err != nil {
			return err
		}
	}

	// Reconcile STONITH after membership changes (other node first, then current).
	if len(nodesToRemove) > 0 || len(nodesToAdd) > 0 {
		fencingNodes := make([]string, 0, 2)
		if otherNodeName != "" {
			fencingNodes = append(fencingNodes, otherNodeName)
		}
		fencingNodes = append(fencingNodes, currentNodeName)
		if err = pcs.ConfigureFencing(ctx, kubeClient, fencingNodes); err != nil {
			return fmt.Errorf("failed to configure fencing: %w", err)
		}
	}

	// Update etcd resource with current node IPs after any membership changes
	// This runs once regardless of whether we removed, added, or both
	if len(nodesToRemove) > 0 || len(nodesToAdd) > 0 {
		nodeIPMap := buildNodeIPMap(cfg)
		command = fmt.Sprintf("/usr/sbin/pcs resource update etcd node_ip_map=\"%s\" --wait=300", nodeIPMap)
		stdOut, stdErr, err := exec.Execute(ctx, command)
		if err != nil {
			klog.Errorf("Failed to update etcd node_ip_map: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
			return err
		}
		klog.Infof("Successfully updated etcd node_ip_map to %q", nodeIPMap)
	}

	// If we added nodes, force new cluster and restart
	// (skip this if we only removed nodes)
	if len(nodesToAdd) > 0 {
		commands := []string{
			// Force new cluster on next etcd restart on this node
			fmt.Sprintf("crm_attribute --lifetime reboot --node %s --name \"force_new_cluster\" --update %s", currentNodeName, currentNodeName),
		}
		if err = runCommands(ctx, commands); err != nil {
			return err
		}

		// Wait for pacemaker resource update to stabilize before starting cluster
		time.Sleep(10 * time.Second)

		commands = []string{
			// Enable cluster on all nodes
			"/usr/sbin/pcs cluster enable --all",
			// Start cluster on all nodes
			"/usr/sbin/pcs cluster start --all",
		}
		if err = runCommands(ctx, commands); err != nil {
			return err
		}
	}

	// Always sync/start at end of this path (cluster was running on this host at start).
	if err := runCommands(ctx, []string{
		"/usr/sbin/pcs cluster sync",
		"/usr/sbin/pcs cluster start --all",
	}); err != nil {
		return fmt.Errorf("pcs cluster sync/start after update-setup: %w", err)
	}
	klog.Info("Completed pcs cluster sync and pcs cluster start --all after update-setup")

	return nil
}

// toSet builds a set from non-empty strings
func toSet(items ...string) map[string]struct{} {
	s := make(map[string]struct{})
	for _, item := range items {
		if item != "" {
			s[item] = struct{}{}
		}
	}
	return s
}

// buildNodeIPMap creates the node_ip_map parameter for pacemaker
func buildNodeIPMap(cfg config.ClusterConfig) string {
	var pairs []string
	if cfg.NodeName1 != "" && cfg.NodeIP1 != "" {
		pairs = append(pairs, fmt.Sprintf("%s:%s", cfg.NodeName1, cfg.NodeIP1))
	}
	if cfg.NodeName2 != "" && cfg.NodeIP2 != "" {
		pairs = append(pairs, fmt.Sprintf("%s:%s", cfg.NodeName2, cfg.NodeIP2))
	}
	return strings.Join(pairs, ";")
}

// pcsClusterNodeRemoveOutputContains401 reports whether pcs remove output suggests PCSD HTTP 401.
func pcsClusterNodeRemoveOutputContains401(stdout, stderr string) bool {
	return strings.Contains(stdout, "401") || strings.Contains(stderr, "401")
}

// removePacemakerNodeGlobally runs pcs cluster node remove. Output containing "401" is logged and ignored.
func removePacemakerNodeGlobally(ctx context.Context, nodeToRemove string) error {
	cmd := fmt.Sprintf("/usr/sbin/pcs cluster node remove %s --force --skip-offline", nodeToRemove)
	stdOut, stdErr, err := exec.Execute(ctx, cmd)
	if err != nil {
		if pcsClusterNodeRemoveOutputContains401(stdOut, stdErr) {
			klog.Warningf(
				"pcs cluster node remove for %q did not succeed and output contained 401: "+
					"the cluster may still see a live endpoint for that member that rejects this node's PCSD credentials — "+
					"commonly a replacement node is answering at the same IP the cluster associates with the old member. "+
					"Skipping fatal error; reconcile sync/start and fencing may still help. stdout=%q stderr=%q err=%v",
				nodeToRemove, stdOut, stdErr, err,
			)
			return nil
		}
		klog.Errorf("pcs cluster node remove failed for %q: stdout=%q stderr=%q err=%v", nodeToRemove, stdOut, stdErr, err)
		return fmt.Errorf("global remove pacemaker node %q: %w (stderr: %s)", nodeToRemove, err, strings.TrimSpace(stdErr))
	}
	klog.Infof("Successfully removed node %q from pacemaker via pcs cluster node remove", nodeToRemove)
	return nil
}

func runCommands(ctx context.Context, commands []string) error {
	for _, command := range commands {
		stdOut, stdErr, err := exec.Execute(ctx, command)
		if err != nil {
			klog.Errorf("Failed to run update-setup command: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
			return err
		}
		klog.Infof("Successfully executed: %s", command)
	}
	return nil
}

// removeUnstartedEtcdMembers removes unstarted etcd members that don't match any current k8s node.
// When a node is removed from pacemaker, its etcd member becomes unstarted (empty name).
// This function removes all such unstarted members whose IP doesn't match any node in currentNodeIPs,
// cleaning up stale members from deleted nodes while preserving members for current nodes.
func removeUnstartedEtcdMembers(ctx context.Context, currentNodeIPs map[string]struct{}) error {
	klog.Info("Checking for unstarted etcd members to remove")

	// Get etcd member list
	command := "podman exec etcd /usr/bin/etcdctl member list -w json"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		return fmt.Errorf("failed to list etcd members: stdout: %s, stderr: %s, err: %w", stdOut, stdErr, err)
	}

	// Parse JSON response
	var memberList struct {
		Members []struct {
			ID         uint64   `json:"ID"`
			Name       string   `json:"name"`
			PeerURLs   []string `json:"peerURLs"`
			ClientURLs []string `json:"clientURLs"`
			IsLearner  bool     `json:"isLearner"`
		} `json:"members"`
	}

	if err := json.Unmarshal([]byte(stdOut), &memberList); err != nil {
		return fmt.Errorf("failed to parse etcd member list JSON: %w", err)
	}

	// Remove all unstarted members that don't match any current node
	var removed []string
	for _, member := range memberList.Members {
		// Only consider unstarted members (empty name indicates unstarted)
		if member.Name != "" {
			continue
		}

		memberIDHex := fmt.Sprintf("%x", member.ID)

		// Extract IP from peer URLs
		var memberIP string
		for _, peerURLStr := range member.PeerURLs {
			parsedURL, err := url.Parse(peerURLStr)
			if err != nil {
				continue
			}

			host, _, err := net.SplitHostPort(parsedURL.Host)
			if err != nil {
				// No port in URL - use host directly and strip IPv6 brackets if present
				host = parsedURL.Host
				if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
					host = host[1 : len(host)-1]
				}
			}
			// SplitHostPort already removes brackets for IPv6 when successful

			// Validate that we got a valid IP
			if net.ParseIP(host) == nil {
				klog.Warningf("Extracted host %q from peer URL %q is not a valid IP", host, peerURLStr)
				continue
			}

			memberIP = host
			break
		}

		if memberIP == "" {
			klog.Warningf("Could not extract IP from unstarted member %s peer URLs: %v", memberIDHex, member.PeerURLs)
			continue
		}

		// Check if this IP matches any current node (use normalized comparison for IPv6)
		matchesCurrentNode := false
		for nodeIP := range currentNodeIPs {
			if ceohelpers.IPAddressesEqual(memberIP, nodeIP) {
				matchesCurrentNode = true
				break
			}
		}
		if matchesCurrentNode {
			klog.Infof("Unstarted etcd member %s (IP: %s) matches a current node - not removing", memberIDHex, memberIP)
			continue
		}

		// This is an unstarted member for a removed/stale node - remove it
		klog.Infof("Found unstarted etcd member %s (IP: %s) not matching any current node - removing", memberIDHex, memberIP)
		command = fmt.Sprintf("podman exec etcd /usr/bin/etcdctl member remove %s", memberIDHex)
		stdOut, stdErr, err = exec.Execute(ctx, command)
		if err != nil {
			return fmt.Errorf("failed to remove unstarted etcd member %s: stdout: %s, stderr: %s, err: %w", memberIDHex, stdOut, stdErr, err)
		}
		removed = append(removed, fmt.Sprintf("%s (IP: %s)", memberIDHex, memberIP))
		klog.Infof("Removed unstarted etcd member %s", memberIDHex)
	}

	if len(removed) == 0 {
		klog.Info("No unstarted etcd members found for removed nodes")
	} else {
		klog.Infof("Successfully removed %d unstarted etcd member(s): %v", len(removed), removed)
	}

	return nil
}

func decodeNodeList(data string) ([]*corev1.Node, error) {
	type nodeInfo struct {
		Name              string               `json:"name"`
		CreationTimestamp metav1.Time          `json:"creationTimestamp"`
		Labels            map[string]string    `json:"labels"`
		Addresses         []corev1.NodeAddress `json:"addresses"`
	}

	var infos []nodeInfo
	if err := json.Unmarshal([]byte(data), &infos); err != nil {
		return nil, err
	}

	nodes := make([]*corev1.Node, len(infos))
	for i, info := range infos {
		nodes[i] = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:              info.Name,
				CreationTimestamp: info.CreationTimestamp,
				Labels:            info.Labels,
			},
			Status: corev1.NodeStatus{
				Addresses: info.Addresses,
			},
		}
	}

	return nodes, nil
}

func getNodeNames(nodes []*corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

func buildClusterConfigFromNodeList(nodes []*corev1.Node) (config.ClusterConfig, error) {
	cfg := config.ClusterConfig{}

	if len(nodes) == 0 || len(nodes) > 2 {
		return cfg, fmt.Errorf("invalid number of nodes: %d", len(nodes))
	}

	// Sort for deterministic ordering
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})

	for i, node := range nodes {
		ip, err := tools.GetNodeIPForPacemaker(*node)
		if err != nil {
			return cfg, err
		}

		switch i {
		case 0:
			cfg.NodeName1 = node.Name
			cfg.NodeIP1 = ip
		case 1:
			cfg.NodeName2 = node.Name
			cfg.NodeIP2 = ip
		}
	}

	return cfg, nil
}

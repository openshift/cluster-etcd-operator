package updatesetup

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
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
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
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
		return fmt.Errorf("pacemaker is not running on this node %s: %w", currentNodeName, err)
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

	klog.Infof("Running update-setup for %d-node cluster", len(capturedNodes))
	klog.V(2).Infof("Captured nodes: %v", getNodeNames(capturedNodes))

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

	// Log operational context (always visible)
	klog.Infof("Running update-setup on node %q (%d-node cluster)", currentNodeName, len(capturedNodes))

	// Log IP details at debug level
	if len(capturedNodes) == 1 {
		klog.V(2).Infof("Single-node mode - Current node: %q (IP: %s)", currentNodeName, currentNodeIP)
	} else {
		klog.V(2).Infof("Two-node mode - Current: %q (IP: %s), Other: %q (IP: %s)", currentNodeName, currentNodeIP, otherNodeName, otherNodeIP)
	}

	// Build desired state from ConfigMap (K8s nodes that should be in pacemaker)
	desiredNodes := make(map[string]string) // nodeName -> IP
	for _, node := range capturedNodes {
		ip, err := tools.GetNodeIPForPacemaker(*node)
		if err != nil {
			klog.Warningf("Skipping node %s from desired state (no valid IP): %v", node.Name, err)
			continue
		}
		desiredNodes[node.Name] = ip
	}

	// Get current pacemaker membership
	// CRITICAL: If we can't determine current state, we MUST NOT make any changes.
	// Making blind changes to pacemaker (node membership, fencing, resources) could brick the cluster
	// by stopping etcd or causing split-brain.
	currentPacemakerNodes, err := getCurrentPacemakerNodesWithIPs(ctx)
	if err != nil {
		klog.Errorf("Cannot determine current pacemaker membership - refusing to make changes to avoid breaking etcd: %v", err)
		return fmt.Errorf("failed to get current pacemaker membership (will retry later, no changes made): %w", err)
	}

	// Determine what needs to change by comparing desired vs current
	nodesToRemove, nodesToAdd := tools.DetermineReconciliationActions(desiredNodes, currentPacemakerNodes)

	klog.Infof("Reconciliation: desired=%d nodes, current=%d nodes, toAdd=%v, toRemove=%v",
		len(desiredNodes), len(currentPacemakerNodes), nodesToAdd, nodesToRemove)

	// Early exit if no changes needed
	if len(nodesToRemove) == 0 && len(nodesToAdd) == 0 {
		klog.Info("No membership changes needed (desired state matches current state)")
		return nil
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
		desiredNodeIPMap := buildNodeIPMap(cfg)

		// Check current node_ip_map to avoid unnecessary updates
		currentNodeIPMap, err := getCurrentNodeIPMap(ctx)
		if err != nil {
			klog.Warningf("Failed to get current node_ip_map (will attempt update): %v", err)
		} else if currentNodeIPMap == desiredNodeIPMap {
			klog.Infof("Skipping node_ip_map update (already set to %q)", desiredNodeIPMap)
		} else {
			klog.Infof("Updating node_ip_map from %q to %q", currentNodeIPMap, desiredNodeIPMap)
			command = fmt.Sprintf("/usr/sbin/pcs resource update etcd node_ip_map=\"%s\" --wait=300", desiredNodeIPMap)
			stdOut, stdErr, err := exec.Execute(ctx, command)
			if err != nil {
				klog.Errorf("Failed to update etcd node_ip_map: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
				return err
			}
			klog.Infof("Successfully updated etcd node_ip_map")
		}
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

	// Register pacemaker alert agents for fencing taint/untaint
	// This is done last so core functionality (node membership, fencing, etcd resource) works even if alerts fail
	// Alerts are optional - don't fail update-setup if they can't be configured
	err = pcs.ConfigureAlerts(ctx)
	if err != nil {
		klog.Warningf("Failed to configure pacemaker alerts (non-fatal, continuing): %v", err)
	} else {
		klog.Infof("Successfully configured pacemaker alerts")
	}

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

// getCurrentNodeIPMap queries the current node_ip_map parameter from the etcd resource
func getCurrentNodeIPMap(ctx context.Context) (string, error) {
	command := "/usr/sbin/pcs resource config etcd"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		return "", fmt.Errorf("failed to get etcd resource config: stdout=%s, stderr=%s, err=%w", stdOut, stdErr, err)
	}

	// Parse output for node_ip_map parameter
	// Expected format: "Attributes: etcd: node_ip_map=master-0:IP;master-1:IP ..."
	lines := strings.Split(stdOut, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "node_ip_map=") {
			// Extract value after node_ip_map=
			parts := strings.SplitN(line, "node_ip_map=", 2)
			if len(parts) == 2 {
				// Value may have quotes and other parameters after it
				value := strings.TrimPrefix(parts[1], "\"")
				value = strings.SplitN(value, "\"", 2)[0] // Get text before closing quote
				value = strings.Fields(value)[0]          // Get first word (handles unquoted case)
				return value, nil
			}
		}
	}

	return "", fmt.Errorf("node_ip_map parameter not found in etcd resource config")
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
			klog.Infof("Unstarted etcd member %s matches a current node - not removing", memberIDHex)
			klog.V(2).Infof("Member %s IP: %s", memberIDHex, memberIP)
			continue
		}

		// This is an unstarted member for a removed/stale node - remove it
		klog.Infof("Found unstarted etcd member %s not matching any current node - removing", memberIDHex)
		klog.V(2).Infof("Member %s IP: %s", memberIDHex, memberIP)
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
		Name string `json:"name"`
		IP   string `json:"ip"`
	}

	var infos []nodeInfo
	if err := json.Unmarshal([]byte(data), &infos); err != nil {
		return nil, err
	}

	nodes := make([]*corev1.Node, len(infos))
	for i, info := range infos {
		// Create minimal node with name and IP address
		nodes[i] = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: info.Name,
			},
			Status: corev1.NodeStatus{
				Addresses: []corev1.NodeAddress{
					{
						Type:    corev1.NodeInternalIP,
						Address: info.IP,
					},
				},
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

// getCurrentPacemakerNodesWithIPs queries pacemaker directly to get current membership with IPs.
// Returns a map of node names to IPs.
// getRuntimePacemakerNodes returns the actual runtime cluster membership from pcs status xml.
// This reflects which nodes are currently configured in the cluster, not the corosync.conf file.
// The config file can be stale (e.g., after a node is fenced), so we use runtime state for reconciliation.
func getRuntimePacemakerNodes(ctx context.Context) (map[string]bool, error) {
	const pcsStatusXMLCommand = "sudo -n pcs status xml"

	ctxExec, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stdout, stderr, err := exec.Execute(ctxExec, pcsStatusXMLCommand)
	if err != nil {
		return nil, fmt.Errorf("failed to execute pcs status xml: %w", err)
	}

	if stderr != "" {
		klog.V(4).Infof("pcs status xml produced stderr: %s", stderr)
	}

	var result pacemaker.PacemakerResult
	if err := xml.Unmarshal([]byte(stdout), &result); err != nil {
		return nil, fmt.Errorf("failed to parse pcs status xml: %w", err)
	}

	// Extract nodes that are members (not remote nodes)
	runtimeNodes := make(map[string]bool)
	for _, node := range result.Nodes.Node {
		if node.Name != "" && node.Type == "member" {
			runtimeNodes[node.Name] = true
		}
	}

	// Build sorted list of node names for logging
	nodeNames := make([]string, 0, len(runtimeNodes))
	for nodeName := range runtimeNodes {
		nodeNames = append(nodeNames, nodeName)
	}
	sort.Strings(nodeNames)

	klog.V(2).Infof("Runtime pacemaker cluster members: %v", nodeNames)
	return runtimeNodes, nil
}

func getCurrentPacemakerNodesWithIPs(ctx context.Context) (map[string]string, error) {
	// Get runtime membership (authoritative source for reconciliation)
	runtimeNodes, err := getRuntimePacemakerNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch runtime cluster membership: %w", err)
	}

	// Get config file (for IP addresses)
	config, err := pacemaker.FetchClusterConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch cluster config: %w", err)
	}

	// Build node map using runtime membership, pulling IPs from config file
	nodeMap := make(map[string]string)
	for nodeName := range runtimeNodes {
		// Find IP from config file
		var nodeIP string
		for _, configNode := range config.Nodes {
			if configNode.Name == nodeName {
				if len(configNode.Addrs) > 0 {
					nodeIP = configNode.Addrs[0].Addr
					break
				}
			}
		}

		if nodeIP == "" {
			// Node in runtime but has no valid IP - mark with empty string
			// DetermineReconciliationActions will detect IP mismatch and remove+add
			klog.Warningf("Node %s in runtime cluster but has no valid IP in config file - will remove and re-add", nodeName)
		}

		// Include all runtime nodes, even those with no IP (using empty string)
		// This triggers removal+re-add for nodes in broken state
		nodeMap[nodeName] = nodeIP
	}

	if len(nodeMap) == 0 {
		return nil, fmt.Errorf("no nodes found in runtime cluster")
	}

	klog.V(2).Infof("Current pacemaker cluster members (runtime + IPs): %v", getMapKeys(nodeMap))
	return nodeMap, nil
}

// getMapKeys returns sorted keys from a map
func getMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

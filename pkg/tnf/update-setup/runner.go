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
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func updateSetupConfigMapSelectsNode(cm *corev1.ConfigMap, nodeName string) bool {
	if cm == nil || cm.Data == nil {
		return false
	}
	if tn, ok := cm.Data["targetNodes"]; ok && strings.TrimSpace(tn) != "" {
		for _, n := range strings.Split(tn, ",") {
			if strings.TrimSpace(n) == nodeName {
				return true
			}
		}
		return false
	}
	return cm.Data["targetNode"] == nodeName
}

func pickUpdateSetupConfigMapForNode(items []corev1.ConfigMap, nodeName string) (*corev1.ConfigMap, error) {
	var best *corev1.ConfigMap
	var bestGen int64 = -1
	for i := range items {
		cm := &items[i]
		if !updateSetupConfigMapSelectsNode(cm, nodeName) {
			continue
		}
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
		return nil, fmt.Errorf("no update-setup ConfigMap found for node %s", nodeName)
	}
	return best, nil
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

	// Get the current node name from environment
	currentNodeName := os.Getenv("MY_NODE_NAME")
	if currentNodeName == "" {
		return fmt.Errorf("MY_NODE_NAME environment variable not set")
	}

	klog.Infof("Running TNF update-setup")

	// check if cluster is running on this node
	command := "/usr/sbin/pcs cluster status"
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		klog.Infof("Cluster not running (err: %v), skipping update-setup on this node", err)
		return nil
	}

	// Pick highest-generation update-setup ConfigMap that targets this node (targetNode or targetNodes).
	cmList, err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=" + tools.TnfUpdateSetupComponentValue,
	})
	if err != nil {
		return fmt.Errorf("failed to list update-setup ConfigMaps: %w", err)
	}

	cm, err := pickUpdateSetupConfigMapForNode(cmList.Items, currentNodeName)
	if err != nil {
		return err
	}

	targetDesc := cm.Data["targetNode"]
	if tn := strings.TrimSpace(cm.Data["targetNodes"]); tn != "" {
		targetDesc = tn
	}
	klog.Infof("Using ConfigMap %s (generation: %s, event: %s, target(s): %s, timestamp: %s)",
		cm.Name, cm.Data["generation"], cm.Data["eventType"], targetDesc, cm.Data["timestamp"])

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

	// Build k8s node identity map (name -> IP)
	k8sNodes := buildNodeMap(cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2)
	currentNodeIPs := toSet(cfg.NodeIP1, cfg.NodeIP2)

	if len(k8sNodes) == 1 {
		klog.Info("Running in single-node mode")
	}

	// Get pacemaker cluster membership with IPs
	command = "/usr/sbin/pcs cluster config show --output-format json"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		klog.Errorf("Failed to get pacemaker cluster config: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
		return err
	}

	// Parse JSON output to get node IPs
	var pacemakerConfig pacemaker.ClusterConfig
	if err := json.Unmarshal([]byte(stdOut), &pacemakerConfig); err != nil {
		klog.Errorf("Failed to parse pacemaker cluster config JSON: %v", err)
		return err
	}

	// Build pacemaker node identity map (name -> IP)
	pacemakerNodes, err := buildPacemakerNodeMap(pacemakerConfig.Nodes)
	if err != nil {
		return fmt.Errorf("failed to build pacemaker node map: %w", err)
	}

	for nodeName, nodeIP := range pacemakerNodes {
		klog.Infof("Found pacemaker node: %q (IP: %s)", nodeName, nodeIP)
	}

	// Determine what changes are needed using reconciliation logic (compares name+IP)
	nodesToRemove, nodesToAdd := reconcileNodes(k8sNodes, pacemakerNodes)

	// Log the decisions (name+IP identity comparison)
	for nodeName, nodeIP := range pacemakerNodes {
		if k8sIP, exists := k8sNodes[nodeName]; !exists {
			klog.Infof("Node %q (IP: %s) is in pacemaker but not in Kubernetes - will remove from pacemaker", nodeName, nodeIP)
		} else if k8sIP != nodeIP {
			klog.Infof("Node %q has IP mismatch (pacemaker: %s, k8s: %s) - will replace", nodeName, nodeIP, k8sIP)
		}
	}
	for nodeName, nodeIP := range k8sNodes {
		if _, exists := pacemakerNodes[nodeName]; !exists {
			klog.Infof("Node %q (IP: %s) is in Kubernetes but not in pacemaker - will add to pacemaker", nodeName, nodeIP)
		}
	}

	if len(nodesToRemove) == 0 && len(nodesToAdd) == 0 {
		klog.Info("Pacemaker membership matches Kubernetes, no membership changes needed")
	}

	klog.Infof("Current node: %q (IP: %s), Other node: %q (IP: %s)", currentNodeName, currentNodeIP, otherNodeName, otherNodeIP)
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

// buildNodeMap creates a map of node name to IP from the cluster config
func buildNodeMap(name1, ip1, name2, ip2 string) map[string]string {
	m := make(map[string]string)
	if name1 != "" && ip1 != "" {
		m[name1] = ip1
	}
	if name2 != "" && ip2 != "" {
		m[name2] = ip2
	}
	return m
}

// buildPacemakerNodeMap creates a map of node name to IP from pacemaker cluster config
func buildPacemakerNodeMap(nodes []pacemaker.ClusterConfigNode) (map[string]string, error) {
	m := make(map[string]string)
	for _, node := range nodes {
		if len(node.Addrs) == 0 {
			return nil, fmt.Errorf("node %q has no addresses in pacemaker config", node.Name)
		}
		// Use the first address (ring0) - matches what we configure during initial setup
		// Initial setup uses: pcs cluster setup TNF <node1> addr=<IP1> <node2> addr=<IP2>
		// This creates a single-ring cluster, so Addrs[0] is always the configured IP
		m[node.Name] = node.Addrs[0].Addr
	}
	return m, nil
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

// reconcileNodes determines which nodes to add/remove based on k8s vs pacemaker membership.
// Treats k8s as the source of truth: nodes in k8s should be in pacemaker, nodes not in k8s should be removed.
// Compares both name AND IP to detect replacements where the name stays the same but IP changes.
func reconcileNodes(k8sNodes, pacemakerNodes map[string]string) (nodesToRemove, nodesToAdd []string) {
	// Remove: pacemaker nodes not in k8s OR with mismatched IPs
	for nodeName, pacemakerIP := range pacemakerNodes {
		k8sIP, existsInK8s := k8sNodes[nodeName]
		if !existsInK8s {
			// Node deleted from k8s
			nodesToRemove = append(nodesToRemove, nodeName)
		} else if k8sIP != pacemakerIP {
			// Node exists in both but IP changed - remove old, add new
			nodesToRemove = append(nodesToRemove, nodeName)
			nodesToAdd = append(nodesToAdd, nodeName)
		}
	}

	// Add: k8s nodes not in pacemaker at all
	for nodeName := range k8sNodes {
		if _, exists := pacemakerNodes[nodeName]; !exists {
			nodesToAdd = append(nodesToAdd, nodeName)
		}
	}

	return nodesToRemove, nodesToAdd
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

		// Check if this IP matches any current node
		if _, isCurrentNode := currentNodeIPs[memberIP]; isCurrentNode {
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

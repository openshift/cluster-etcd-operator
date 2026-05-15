package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	// Node event types for update-setup reconciliation
	NodeEventTypeAdd    = "add"
	NodeEventTypeReady  = "ready"
	NodeEventTypeDelete = "delete"
)

var (
	// handleNodesMutex ensures that node handling code doesn't run concurrently
	handleNodesMutex sync.Mutex

	// updateSetupGeneration orders update-setup ConfigMaps when events arrive close together (OCPBUGS-84695).
	updateSetupGeneration      int64 = 0
	updateSetupGenerationMutex sync.Mutex

	// handleNodesFunc is a variable to allow mocking in tests
	handleNodesFunc = handleNodes

	// startTnfJobcontrollersFunc is a variable to allow mocking in tests
	startTnfJobcontrollersFunc = startTnfJobcontrollers

	// updateSetupFunc is a variable to allow mocking in tests
	updateSetupFunc = updateSetup

	// retryBackoffConfig allows customizing retry behavior for tests
	retryBackoffConfig = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Steps:    9, // ~10 minutes total: 5s + 10s + 20s + 40s + 80s + 120s + 120s + 120s + 120s
		Cap:      2 * time.Minute,
	}
)

func handleNodesWithRetry(
	controllerContext *controllercmd.ControllerContext,
	controlPlaneNodeLister corev1listers.NodeLister,
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
	triggeringNode *corev1.Node,
	eventType string,
) {

	// Ensure only one execution at a time - don't run in parallel
	handleNodesMutex.Lock()
	defer handleNodesMutex.Unlock()

	// Retry with exponential backoff to handle transient failures
	var setupErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoffConfig, func(ctx context.Context) (bool, error) {
		setupErr = handleNodesFunc(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer, triggeringNode, eventType)
		if setupErr != nil {
			klog.Warningf("failed to setup TNF job controllers, will retry: %v", setupErr)
			return false, nil
		}
		return true, nil
	})

	if err != nil || setupErr != nil {
		klog.Errorf("failed to setup TNF job controllers after 10 minutes of retries: %v", setupErr)

		// Degrade the operator to indicate TNF job controller setup failed
		_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "TNFJobControllersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SetupFailed",
			Message: fmt.Sprintf("Failed to setup TNF job controllers after retries: %v", setupErr),
		}))
		if updateErr != nil {
			klog.Errorf("failed to update operator status to degraded: %v", updateErr)
		}
	} else {
		// Clear any previous degraded condition on success
		_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "TNFJobControllersDegraded",
			Status:  operatorv1.ConditionFalse,
			Reason:  "AsExpected",
			Message: "TNF job controllers setup completed successfully",
		}))
		if updateErr != nil {
			klog.Errorf("failed to update operator status: %v", updateErr)
		}
	}
}

func handleNodes(
	controllerContext *controllercmd.ControllerContext,
	controlPlaneNodeLister corev1listers.NodeLister,
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
	triggeringNode *corev1.Node,
	eventType string,
) error {

	// Get control plane nodes
	nodeList, err := controlPlaneNodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	// Validate node count for unsupported scenarios
	if len(nodeList) > 3 {
		klog.Warningf("found more than 3 control plane nodes (%d), unsupported use case, no further steps are taken", len(nodeList))
		return nil
	}
	if len(nodeList) < 1 {
		klog.Warningf("no control plane nodes found, waiting for nodes")
		return nil
	}

	// Add/delete post-handoff paths consult ExternalEtcdHasCompletedTransition; Ready does not run update-setup.
	var externalEtcdTransitionComplete bool
	if eventType == NodeEventTypeAdd || eventType == NodeEventTypeDelete {
		var err error
		externalEtcdTransitionComplete, err = ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
		if err != nil {
			return fmt.Errorf("failed to check external etcd transition status: %w", err)
		}
	}

	// Handle different event types
	switch eventType {
	case NodeEventTypeAdd:
		return handleNodeAddition(nodeList, triggeringNode, externalEtcdTransitionComplete, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	case NodeEventTypeReady:
		return handleNodeReady(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	case NodeEventTypeDelete:
		return handleNodeDeletion(nodeList, triggeringNode, externalEtcdTransitionComplete, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	default:
		return fmt.Errorf("unknown event type: %s", eventType)
	}
}

// handleNodeAddition runs update-setup on the peer of the triggering node once handoff is complete and both nodes are Ready.
func handleNodeAddition(
	nodeList []*corev1.Node,
	triggeringNode *corev1.Node,
	externalEtcdTransitionComplete bool,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) error {
	// Initial install: handoff not done yet — ready events drive startTnfJobcontrollers; ignore add noise.
	if !externalEtcdTransitionComplete {
		klog.Infof("Node %s added, but external etcd transition not complete yet - waiting for ready events for initial setup", triggeringNode.Name)
		return nil
	}

	// TNF initial handoff complete — adding node to existing cluster
	nodeCount := len(nodeList)
	if nodeCount > 2 {
		klog.Warningf("Node %s added, but found %d nodes total - more than 2 nodes is undefined, not running update-setup", triggeringNode.Name, nodeCount)
		return nil
	}

	if nodeCount < 2 {
		klog.Infof("Node %s added, but only %d node(s) in cluster - waiting for second node", triggeringNode.Name, nodeCount)
		return nil
	}

	// We have 2 nodes - verify both are ready before running update-setup
	allReady := true
	for _, node := range nodeList {
		if !tools.IsNodeReady(node) {
			klog.Infof("Node %s added, but node %s is not ready yet - waiting for both nodes to be ready", triggeringNode.Name, node.Name)
			allReady = false
			break
		}
	}
	if !allReady {
		// UpdateFunc will fire when the not-ready node becomes ready
		return nil
	}

	klog.Infof("Node %s added to existing cluster, both nodes ready (%s, %s) - running update-setup on original node",
		triggeringNode.Name, nodeList[0].Name, nodeList[1].Name)

	// Always start job controllers for the new node
	err := startTnfJobcontrollersFunc(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	if err != nil {
		return fmt.Errorf("failed to start TNF job controllers: %w", err)
	}

	// Run update-setup to reconcile pacemaker membership
	err = updateSetupFunc(nodeList, triggeringNode, NodeEventTypeAdd, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
	if err != nil {
		return fmt.Errorf("failed to update pacemaker setup for node addition: %w", err)
	}

	return nil
}

// handleNodeReady: two ready masters → startTnfJobcontrollers (historical invariant). Never Job-list gate here—fencing can exist early via secrets (OCPBUGS-84695).
func handleNodeReady(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) error {
	// Initial setup: need 2 ready nodes
	nodeCount := len(nodeList)
	if nodeCount < 2 {
		klog.Infof("Node became ready, but only %d node(s) in cluster - waiting for 2 nodes for initial TNF setup", nodeCount)
		return nil
	}

	// Check that both nodes are ready
	allReady := true
	for _, node := range nodeList {
		if !tools.IsNodeReady(node) {
			klog.Infof("Node became ready, but node %s is not ready yet - waiting for both nodes", node.Name)
			allReady = false
			break
		}
	}
	if !allReady {
		return nil
	}

	klog.Infof("Both nodes ready (%s, %s) - starting initial TNF setup", nodeList[0].Name, nodeList[1].Name)

	// Start job controllers for initial setup
	err := startTnfJobcontrollersFunc(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	if err != nil {
		return fmt.Errorf("failed to start TNF job controllers for initial setup: %w", err)
	}

	// Initial setup job will handle pacemaker cluster creation
	return nil
}

// controlPlaneNodesExceptName returns listed control-plane nodes excluding excludeName (delete survivor set).
// Delete path returns an error until survivors are Ready so handleNodesWithRetry keeps backing off.
func controlPlaneNodesExceptName(nodes []*corev1.Node, excludeName string) []*corev1.Node {
	out := make([]*corev1.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.Name == excludeName {
			continue
		}
		out = append(out, n)
	}
	return out
}

// handleNodeDeletion runs update-setup after delete: survivor if one node left, else lexicographically first among remaining.
func handleNodeDeletion(
	nodeList []*corev1.Node,
	triggeringNode *corev1.Node,
	externalEtcdTransitionComplete bool,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) error {
	// Before handoff there is no pacemaker-managed cluster to reconcile.
	if !externalEtcdTransitionComplete {
		klog.Infof("Node %s deleted, but external etcd transition not complete yet - no action needed", triggeringNode.Name)
		return nil
	}

	remaining := controlPlaneNodesExceptName(nodeList, triggeringNode.Name)

	// Require all survivors Ready before update-setup (delete has no later reconcile once at one node).
	allReady := true
	for _, node := range remaining {
		if !tools.IsNodeReady(node) {
			klog.Infof("Node %s deleted, but node %s is not ready yet - waiting for remaining nodes to be ready", triggeringNode.Name, node.Name)
			allReady = false
			break
		}
	}
	if !allReady {
		return fmt.Errorf("node %s deleted: waiting for remaining control-plane nodes to be Ready before pacemaker update-setup", triggeringNode.Name)
	}

	klog.Infof("Node %s deleted, %d node(s) remaining - running update-setup to reconcile pacemaker membership", triggeringNode.Name, len(remaining))

	// Always start job controllers (ensures controllers exist for remaining nodes)
	err := startTnfJobcontrollersFunc(remaining, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	if err != nil {
		return fmt.Errorf("failed to start TNF job controllers: %w", err)
	}

	// Run update-setup to remove deleted node from pacemaker
	err = updateSetupFunc(remaining, triggeringNode, NodeEventTypeDelete, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
	if err != nil {
		return fmt.Errorf("failed to update pacemaker setup for node deletion: %w", err)
	}

	return nil
}

func getNextUpdateSetupGeneration() int64 {
	updateSetupGenerationMutex.Lock()
	defer updateSetupGenerationMutex.Unlock()
	updateSetupGeneration++
	return updateSetupGeneration
}

// updateSetup writes a snapshot ConfigMap, runs auth on all nodes, update-setup on one target, then after-setup on all.
func updateSetup(
	nodeList []*corev1.Node,
	triggeringNode *corev1.Node,
	eventType string,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) error {

	generation := getNextUpdateSetupGeneration()

	targetNode := determineUpdateSetupTargetNode(nodeList, triggeringNode, eventType)
	if targetNode == nil {
		return fmt.Errorf("failed to determine target node for update-setup (event: %s, triggering: %s, generation: %d)",
			eventType, triggeringNode.Name, generation)
	}

	klog.Infof("Generation %d: Event=%s, Triggering=%s, Target=%s, NodeList=%v",
		generation, eventType, triggeringNode.Name, targetNode.Name, getNodeNames(nodeList))

	// Snapshot nodeList into ConfigMap for the runner (see comment on updateSetup).
	nodeListData, err := encodeNodeList(nodeList)
	if err != nil {
		return fmt.Errorf("failed to encode node list: %w", err)
	}

	cmName := fmt.Sprintf("tnf-update-setup-%d", generation)
	cmData := map[string]string{
		"nodes":      nodeListData,
		"generation": fmt.Sprintf("%d", generation),
		"eventType":  eventType,
		"timestamp":  time.Now().Format(time.RFC3339),
		"targetNode": targetNode.Name,
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Name:      cmName,
			Namespace: operatorclient.TargetNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/component":      tools.TnfUpdateSetupComponentValue,
				"tnf.etcd.openshift.io/generation": fmt.Sprintf("%d", generation),
			},
		},
		Data: cmData,
	}

	_, err = kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Create(ctx, cm, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create ConfigMap %s: %w", cmName, err)
	}

	// Clean up ConfigMap after all jobs complete
	defer func() {
		if err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Delete(ctx, cmName, v1.DeleteOptions{}); err != nil {
			klog.Warningf("failed to delete ConfigMap %s: %v", cmName, err)
		}
	}()

	// Run auth jobs on all nodes to ensure pacemaker authentication is current
	if err := runJobsOnNodes(ctx, tools.JobTypeAuth, nodeList, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces); err != nil {
		return err
	}

	// Single orchestrator per reconcile (ConfigMap generation is per call).
	if err := runJobsOnNodes(ctx, tools.JobTypeUpdateSetup, []*corev1.Node{targetNode}, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces); err != nil {
		return err
	}

	// Run after-setup jobs on all nodes for post-reconciliation tasks
	if err := runJobsOnNodes(ctx, tools.JobTypeAfterSetup, nodeList, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces); err != nil {
		return err
	}

	return nil
}

// determineUpdateSetupTargetNode returns the one node that runs the update-setup job (add: peer; delete: survivor or lex-first).
func determineUpdateSetupTargetNode(nodeList []*corev1.Node, triggeringNode *corev1.Node, eventType string) *corev1.Node {
	switch eventType {
	case NodeEventTypeAdd:
		// Adding node to existing cluster - run on the original node (peer of the new node)
		if len(nodeList) != 2 {
			klog.Warningf("Add event but found %d nodes, expected 2", len(nodeList))
			return nil
		}
		for _, node := range nodeList {
			if node.Name != triggeringNode.Name {
				klog.Infof("Add event: selecting original node %s (peer of new node %s)", node.Name, triggeringNode.Name)
				return node
			}
		}
		klog.Warningf("Add event - couldn't find peer node for triggering node %s", triggeringNode.Name)
		return nil

	case NodeEventTypeDelete:
		nodeCount := len(nodeList)
		if nodeCount == 0 {
			klog.Warningf("Delete event but no nodes remaining")
			return nil
		}
		if nodeCount == 1 {
			klog.Infof("Delete event: selecting survivor node %s", nodeList[0].Name)
			return nodeList[0]
		}
		selectedNode := nodeList[0]
		for _, node := range nodeList[1:] {
			if node.Name < selectedNode.Name {
				selectedNode = node
			}
		}
		klog.Infof("Delete event: selecting orchestrator node %s (alphabetically first of %d remaining nodes)", selectedNode.Name, nodeCount)
		return selectedNode

	default:
		klog.Warningf("determineUpdateSetupTargetNode: unsupported event type %s", eventType)
		return nil
	}
}

func getNodeNames(nodes []*corev1.Node) []string {
	names := make([]string, len(nodes))
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}

func runJobsOnNodes(
	ctx context.Context,
	jobType tools.JobType,
	nodes []*corev1.Node,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) error {
	timeout := getJobTimeout(jobType)

	for _, node := range nodes {
		if err := jobs.RestartJobOrRunController(ctx, jobType, &node.Name,
			controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces,
			jobs.DefaultConditions, timeout); err != nil {
			return fmt.Errorf("failed to start %s job on node %s: %w", jobType.GetSubCommand(), node.Name, err)
		}
	}

	for _, node := range nodes {
		if err := jobs.WaitForCompletion(ctx, kubeClient, jobType.GetJobName(&node.Name),
			operatorclient.TargetNamespace, timeout); err != nil {
			return fmt.Errorf("failed to wait for %s job on node %s: %w", jobType.GetSubCommand(), node.Name, err)
		}
	}

	return nil
}

func getJobTimeout(jobType tools.JobType) time.Duration {
	switch jobType {
	case tools.JobTypeAuth:
		return tools.AuthJobCompletedTimeout
	case tools.JobTypeUpdateSetup:
		return tools.SetupJobCompletedTimeout
	case tools.JobTypeAfterSetup:
		return tools.AfterSetupJobCompletedTimeout
	default:
		return tools.AllCompletedTimeout
	}
}

func encodeNodeList(nodes []*corev1.Node) (string, error) {
	type nodeInfo struct {
		Name              string               `json:"name"`
		CreationTimestamp v1.Time              `json:"creationTimestamp"`
		Labels            map[string]string    `json:"labels"`
		Addresses         []corev1.NodeAddress `json:"addresses"`
	}

	infos := make([]nodeInfo, len(nodes))
	for i, node := range nodes {
		infos[i] = nodeInfo{
			Name:              node.Name,
			CreationTimestamp: node.CreationTimestamp,
			Labels:            node.Labels,
			Addresses:         node.Status.Addresses,
		}
	}

	data, err := json.Marshal(infos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func startTnfJobcontrollers(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) error {

	klog.Infof("Running TNF setup procedure. Waiting for etcd bootstrap to complete")

	// Wait for the etcd informer to sync before checking bootstrap status
	// This ensures operatorClient.GetStaticPodOperatorState() has data to work with
	klog.Infof("waiting for etcd informer to sync...")
	if !cache.WaitForCacheSync(ctx.Done(), etcdInformer.Informer().HasSynced) {
		return fmt.Errorf("failed to sync etcd informer")
	}
	klog.Infof("etcd informer synced")

	if err := waitForEtcdBootstrapCompleted(ctx, operatorClient); err != nil {
		return fmt.Errorf("failed to wait for etcd bootstrap: %w", err)
	}

	// Wait for all nodes to have their installers complete (creates /var/lib/etcd)
	klog.Infof("bootstrap completed, waiting for all nodes to reach latest revision")
	if err := etcd.WaitForStableRevision(ctx, operatorClient); err != nil {
		return fmt.Errorf("failed to wait for all nodes at latest revision: %w", err)
	}

	klog.Infof("all nodes at latest revision, creating TNF job controllers")

	// the order of job creation does not matter, the jobs wait on each other as needed
	for _, node := range nodeList {
		jobs.RunTNFJobController(ctx, tools.JobTypeAuth, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		jobs.RunTNFJobController(ctx, tools.JobTypeAfterSetup, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
	}

	jobs.RunTNFJobController(ctx, tools.JobTypeSetup, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.AllConditions)
	jobs.RunTNFJobController(ctx, tools.JobTypeFencing, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

	// wait until the after-setup jobs finished,
	// in order to avoid races with update jobs
	return waitForTnfAfterSetupJobsCompletion(ctx, kubeClient, nodeList)
}

func waitForEtcdBootstrapCompleted(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient) error {
	isEtcdRunningInCluster, err := ceohelpers.IsEtcdRunningInCluster(ctx, operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check if bootstrap is completed: %w", err)
	}
	if !isEtcdRunningInCluster {
		klog.Infof("waiting for bootstrap to complete with etcd running in cluster")
		clientConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		err = bootstrapteardown.WaitForEtcdBootstrap(ctx, clientConfig)
		if err != nil {
			return fmt.Errorf("failed to wait for bootstrap to complete: %w", err)
		}
	}
	return nil
}

func waitForTnfAfterSetupJobsCompletion(ctx context.Context, kubeClient kubernetes.Interface, nodeList []*corev1.Node) error {
	for _, node := range nodeList {
		jobName := tools.JobTypeAfterSetup.GetJobName(&node.Name)
		klog.Infof("Waiting for after-setup job %s to complete", jobName)
		if err := jobs.WaitForCompletion(ctx, kubeClient, jobName, operatorclient.TargetNamespace, tools.AllCompletedTimeout); err != nil {
			return fmt.Errorf("failed to wait for after-setup job %s to complete: %w", jobName, err)
		}
	}
	return nil
}

package operator

import (
	"context"
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
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

var (
	// handleNodesMutex ensures that node handling code doesn't run concurrently
	handleNodesMutex sync.Mutex

	// handleNodesFunc is a variable to allow mocking in tests
	handleNodesFunc = handleNodes

	// retryBackoffConfig allows customizing retry behavior for tests
	retryBackoffConfig = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Steps:    9, // ~10 minutes total: 5s + 10s + 20s + 40s + 80s + 120s + 120s + 120s + 120s
		Cap:      2 * time.Minute,
	}

	// updateSetupJobWaitTimeout is the maximum time to wait for the update-setup job to complete
	updateSetupJobWaitTimeout = 10 * time.Minute
)

func handleNodesWithRetry(
	controllerContext *controllercmd.ControllerContext,
	controlPlaneNodeLister corev1listers.NodeLister,
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) {

	// Ensure only one execution at a time - don't run in parallel
	handleNodesMutex.Lock()
	defer handleNodesMutex.Unlock()

	// Retry with exponential backoff to handle transient failures
	var setupErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoffConfig, func(ctx context.Context) (bool, error) {
		setupErr = handleNodesFunc(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
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
) error {

	// ensure we have 2 control plane nodes before doing anything
	nodeList, err := controlPlaneNodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}
	if len(nodeList) > 2 {
		klog.Errorf("found more than 2 control plane nodes (%d), unsupported use case, no further steps are taken for now", len(nodeList))
		// don't retry
		return nil
	}
	if len(nodeList) < 2 {
		klog.Warningf("found a single control plane node only, waiting for the second one")
		// don't retry
		return nil
	}
	bothReady := true
	for _, node := range nodeList {
		if !tools.IsNodeReady(node) {
			klog.Warningf("node %q is not ready yet, waiting for it to become ready", node.GetName())
			bothReady = false
		}
	}
	if !bothReady {
		return nil
	}

	klog.Infof("found 2 control plane nodes (%q, %q)", nodeList[0].GetName(), nodeList[1].GetName())

	// check if TNF was already set up, by looking for existing jobs
	jobsExist, err := tnfSetupJobsExist(ctx, kubeClient)
	if err != nil {
		return fmt.Errorf("failed to check for existing TNF jobs: %w", err)
	}

	// always start job controllers, otherwise jobs won't be recreated after a CEO restart
	err = startTnfJobcontrollers(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	if err != nil {
		return fmt.Errorf("failed to start TNF job controllers: %w", err)
	}

	// in case TNF was not setup yet, we are done here
	if !jobsExist {
		return nil
	}

	// if TNF was already set up, we might need to update
	err = updateSetup(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
	if err != nil {
		return fmt.Errorf("failed to update pacemaker setup: %w", err)
	}

	return nil
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
	klog.Infof("bootstrap completed, creating TNF job controllers")

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
		return fmt.Errorf("failed to check if bootstrap is completed: %v", err)
	}
	if !isEtcdRunningInCluster {
		klog.Infof("waiting for bootstrap to complete with etcd running in cluster")
		clientConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to get in-cluster config: %v", err)
		}
		err = bootstrapteardown.WaitForEtcdBootstrap(ctx, clientConfig)
		if err != nil {
			return fmt.Errorf("failed to wait for bootstrap to complete: %v", err)
		}
	}
	return nil
}

func updateSetup(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) error {

	klog.Info("TNF was already setup, checking for needed changes for modified nodes")

	// re-run auth job on both nodes
	for _, node := range nodeList {
		klog.Infof("(Re-)running auth job on node %s", node.GetName())
		err := jobs.RestartJobOrRunController(ctx, tools.JobTypeAuth, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.AuthJobCompletedTimeout)
		if err != nil {
			return fmt.Errorf("failed to (re-)start auth job on node %s: %w", node.GetName(), err)
		}
	}
	// wait for completion
	for _, node := range nodeList {
		err := jobs.WaitForCompletion(ctx, kubeClient, tools.JobTypeAuth.GetJobName(&node.Name), operatorclient.TargetNamespace, tools.AuthJobCompletedTimeout)
		if err != nil {
			return fmt.Errorf("failed to wait for auth job on node %s to complete: %w", node.Name, err)
		}
	}

	// run update-setup job on both nodes, it will detect on which node it needs to do something
	for _, node := range nodeList {
		klog.Infof("(Re-)running update setup job on node %s", node.GetName())
		err := jobs.RestartJobOrRunController(ctx, tools.JobTypeUpdateSetup, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.SetupJobCompletedTimeout)
		if err != nil {
			return fmt.Errorf("failed to (re-)start update setup job on node %s: %w", node.GetName(), err)
		}

	}
	// wait for completion
	for _, node := range nodeList {
		err := jobs.WaitForCompletion(ctx, kubeClient, tools.JobTypeUpdateSetup.GetJobName(&node.Name), operatorclient.TargetNamespace, tools.SetupJobCompletedTimeout)
		if err != nil {
			return fmt.Errorf("failed to wait for update setup job on node %s to complete: %w", node.GetName(), err)
		}
	}

	// run after-setup job on both nodes
	for _, node := range nodeList {
		klog.Infof("(Re-)running after setup job on node %s", node.GetName())
		err := jobs.RestartJobOrRunController(ctx, tools.JobTypeAfterSetup, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.AfterSetupJobCompletedTimeout)
		if err != nil {
			return fmt.Errorf("failed to (re-)start after setup job on node %s: %w", node.GetName(), err)
		}

	}
	// wait for completion
	for _, node := range nodeList {
		err := jobs.WaitForCompletion(ctx, kubeClient, tools.JobTypeAfterSetup.GetJobName(&node.Name), operatorclient.TargetNamespace, tools.AfterSetupJobCompletedTimeout)
		if err != nil {
			return fmt.Errorf("failed to wait for after setup job on node %s to complete: %w", node.GetName(), err)
		}
	}

	// rerun fencing job
	err := jobs.RestartJobOrRunController(ctx, tools.JobTypeFencing, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.FencingJobCompletedTimeout)
	if err != nil {
		return fmt.Errorf("failed to restart fencing job: %w", err)
	}

	return nil
}

// tnfSetupJobsExist checks if TNF was already set up by checking for any existing TNF jobs
func tnfSetupJobsExist(ctx context.Context, kubeClient kubernetes.Interface) (bool, error) {
	// Check if any TNF jobs exist in the target namespace
	jobsClient := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace)
	list, err := jobsClient.List(ctx, v1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=two-node-fencing-setup",
	})
	if err != nil {
		return false, err
	}
	return len(list.Items) > 0, nil
}

func waitForTnfAfterSetupJobsCompletion(ctx context.Context, kubeClient kubernetes.Interface, nodeList []*corev1.Node) error {
	for _, node := range nodeList {
		jobName := tools.JobTypeAfterSetup.GetJobName(&node.Name)
		klog.Infof("Waiting for after-setup job %s to complete", jobName)
		if err := jobs.WaitForCompletion(ctx, kubeClient, jobName, operatorclient.TargetNamespace, 20*time.Minute); err != nil {
			return fmt.Errorf("failed to wait for after-setup job %s to complete: %w", jobName, err)
		}
	}
	return nil
}

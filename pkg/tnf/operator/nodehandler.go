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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

func handleAddedNode(
	controllerContext *controllercmd.ControllerContext,
	controlPlaneNodeLister corev1listers.NodeLister,
	once *sync.Once,
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) func(obj interface{}) {

	return func(obj interface{}) {
		node, ok := obj.(*corev1.Node)
		if !ok {
			klog.Warningf("failed to convert added object to Node %+v", obj)
			return
		}
		klog.Infof("node added: %s", node.GetName())

		// ensure we have both control plane nodes before creating jobs
		nodeList, err := controlPlaneNodeLister.List(labels.Everything())
		if err != nil {
			klog.Errorf("failed to list control plane nodes while waiting to create TNF jobs: %v", err)
			return
		}
		if len(nodeList) != 2 {
			klog.Info("not starting TNF jobs yet, waiting for 2 control plane nodes to exist")
			return
		}
		klog.Infof("found 2 control plane nodes (%q, %q)", nodeList[0].GetName(), nodeList[1].GetName())

		// we can have 2 nodes on the first call of AddFunc already, ensure we create job controllers once only
		once.Do(func() {
			// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
			// to not block the event handler
			go handleNodesWithRetry(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		})
	}
}

func handleNodesWithRetry(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) {

	// Retry with exponential backoff to handle transient failures
	var setupErr error
	err := wait.ExponentialBackoffWithContext(ctx, retryBackoffConfig, func(ctx context.Context) (bool, error) {
		setupErr = handleNodesFunc(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
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
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) error {

	if err := ceohelpers.WaitForEtcdCondition(
		ctx,
		etcdInformer,
		operatorClient,
		ceohelpers.IsEtcdRunningInCluster,
		10*time.Second,
		30*time.Minute,
		"etcd bootstrap completion",
	); err != nil {
		return fmt.Errorf("failed to wait for etcd bootstrap: %w", err)
	}
	klog.Infof("bootstrap completed, creating TNF job controllers")

	// the order of job creation does not matter, the jobs wait on each other as needed
	for _, node := range nodeList {
		runJobController(ctx, tools.JobTypeAuth, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
		runJobController(ctx, tools.JobTypeAfterSetup, &node.Name, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)
	}

	hasExternalEtcdCompletedTransition, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
	if err != nil {
		klog.Errorf("failed to get external etcd transition status; proceeding as though it has not transitioned: %v", err)
		hasExternalEtcdCompletedTransition = false
	}

	setupConditions := jobs.DefaultConditions
	if !hasExternalEtcdCompletedTransition {
		setupConditions = jobs.AllConditions
	}

	runJobController(ctx, tools.JobTypeSetup, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, setupConditions)
	runJobController(ctx, tools.JobTypeFencing, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions)

	return nil
}

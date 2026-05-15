package operator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/externaletcdsupportcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

var (
	// fencingUpdateTriggered is set to true when a fencing update is already triggered
	fencingUpdateTriggered bool
	// fencingUpdateMutex is used to make usage of fencingUpdateTriggered thread safe
	fencingUpdateMutex sync.Mutex

	handleNodesFunc = handleNodes // test hook

	// Per wave: exponential backoff, then Degraded + checkpoint (OCPBUGS-84695).
	retryBackoffConfig = wait.Backoff{
		Duration: 5 * time.Second,
		Factor:   2.0,
		Steps:    9,
		Cap:      2 * time.Minute,
	}

	// Between waves after backoff exhaustion (OCPBUGS-84695); tests shorten.
	tnfJobControllerSetupCheckpointInterval = 10 * time.Minute
)

const (
	masterNodeLabelKey       = "node-role.kubernetes.io/master"
	controlPlaneNodeLabelKey = "node-role.kubernetes.io/control-plane"
)

// hasControlPlaneRoleLabels is true when the node has master or control-plane role labels.
// Arbiter nodes are not full control-plane nodes and do not carry these labels; other call sites must union arbiter when needed.
func hasControlPlaneRoleLabels(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	l := node.Labels
	_, master := l[masterNodeLabelKey]
	_, controlPlane := l[controlPlaneNodeLabelKey]
	return master || controlPlane
}

func filterControlPlaneNodesForTNF(nodes []*corev1.Node) []*corev1.Node {
	out := make([]*corev1.Node, 0, len(nodes))
	for _, n := range nodes {
		if hasControlPlaneRoleLabels(n) {
			out = append(out, n)
		}
	}
	return out
}

// HandleDualReplicaClusters checks feature gate and control plane topology,
// and handles dual replica aka two node fencing clusters
func HandleDualReplicaClusters(
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	infrastructureInformer configv1informers.InfrastructureInformer,
	operatorClient v1helpers.StaticPodOperatorClient,
	envVarGetter etcdenvvar.EnvVar,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	networkInformer configv1informers.NetworkInformer,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	etcdInformer operatorv1informers.EtcdInformer,
	kubeClient kubernetes.Interface,
	dynamicClient dynamic.Interface) (bool, error) {

	// Since HandleDualReplicaClusters isn't a controller, we need to ensure that the Infrastructure
	// informer is synced before we use it.
	if !cache.WaitForCacheSync(ctx.Done(), infrastructureInformer.Informer().HasSynced) {
		return false, fmt.Errorf("failed to sync Infrastructure informer")
	}
	isExternalEtcdCluster, err := ceohelpers.IsExternalEtcdCluster(ctx, infrastructureInformer.Lister())
	if err != nil {
		klog.Errorf("failed to check if external etcd cluster is enabled: %v", err)
		return false, err
	}
	if !isExternalEtcdCluster {
		return false, nil
	}

	klog.Infof("starting Two Node Fencing controllers")

	runExternalEtcdSupportController(ctx, controllerContext, operatorClient, envVarGetter, kubeInformersForNamespaces,
		infrastructureInformer, networkInformer, controlPlaneNodeInformer, etcdInformer, kubeClient)
	runTnfResourceController(ctx, controllerContext, kubeClient, dynamicClient, operatorClient, kubeInformersForNamespaces)

	controlPlaneNodeLister := corev1listers.NewNodeLister(controlPlaneNodeInformer.GetIndexer())

	// we need node names for assigning auth and after-setup jobs to specific nodes
	klog.Infof("watching for nodes...")
	var once sync.Once
	_, err = controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: handleAddedNode(controllerContext, controlPlaneNodeLister, &once, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer),
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to control plane informer: %v", err)
		return false, err
	}

	// we need to update fencing config when fencing secrets change
	// adding a secret informer to the jobcontroller would trigger fencing setup for *every* secret change,
	// that's why we do it with an event handler
	klog.Infof("watching for secrets...")
	_, err = kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			handleFencingSecretChange(ctx, operatorClient, kubeClient, nil, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			handleFencingSecretChange(ctx, operatorClient, kubeClient, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			handleFencingSecretChange(ctx, operatorClient, kubeClient, nil, obj)
		},
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to secrets informer: %v", err)
		return false, err
	}

	return true, nil
}

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
		tryStartTnfJobControllers(controllerContext, controlPlaneNodeLister, once, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	}
}

func tryStartTnfJobControllers(
	controllerContext *controllercmd.ControllerContext,
	controlPlaneNodeLister corev1listers.NodeLister,
	once *sync.Once,
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) {
	nodeList, err := controlPlaneNodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list control plane nodes while waiting to create TNF jobs: %v", err)
		return
	}
	nodes := filterControlPlaneNodesForTNF(nodeList)
	if len(nodes) != 2 {
		if len(nodes) > 2 {
			klog.Warningf("found %d control-plane role nodes; TNF expects 2, not starting TNF jobs", len(nodes))
		} else {
			klog.V(4).Infof("TNF jobs: need 2 control-plane role nodes, have %d", len(nodes))
		}
		return
	}
	klog.Infof("found 2 control plane nodes for TNF (%q, %q)", nodes[0].Name, nodes[1].Name)

	once.Do(func() {
		freshList, listErr := controlPlaneNodeLister.List(labels.Everything())
		if listErr != nil {
			klog.Errorf("failed to list control plane nodes for TNF job startup: %v", listErr)
			return
		}
		fresh := filterControlPlaneNodesForTNF(freshList)
		if len(fresh) != 2 {
			klog.Warningf("expected 2 control-plane role nodes at TNF startup, got %d", len(fresh))
			return
		}
		go handleNodesWithRetry(fresh, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
	})
}

func handleNodesWithRetry(
	nodeList []*corev1.Node,
	ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer) {

	// Exponential backoff per wave; Degraded + checkpoint on exhaustion (OCPBUGS-84695).
	for {
		if ctx.Err() != nil {
			klog.Infof("stopping TNF job controller setup: %v", ctx.Err())
			return
		}

		waveBackoff := retryBackoffConfig
		var lastSetupErr error
		err := wait.ExponentialBackoffWithContext(ctx, waveBackoff, func(ctx context.Context) (bool, error) {
			setupErr := handleNodesFunc(nodeList, ctx, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
			if setupErr == nil {
				return true, nil
			}
			lastSetupErr = setupErr
			klog.Warningf("TNF job controller setup failed, retrying: %v", setupErr)
			return false, nil
		})

		if err == nil {
			// Clear TNFJobControllersDegraded after setup succeeds (e.g. following an earlier failed wave).
			_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    "TNFJobControllersDegraded",
				Status:  operatorv1.ConditionFalse,
				Reason:  "AsExpected",
				Message: "TNF job controllers setup completed successfully",
			}))
			if updateErr != nil {
				klog.Errorf("failed to update operator status: %v", updateErr)
			}
			return
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			klog.Infof("stopping TNF job controller setup: %v", err)
			return
		}

		msg := fmt.Sprintf("TNF setup: backoff error %v; pauses %v then retries", err, tnfJobControllerSetupCheckpointInterval)
		if errors.Is(err, wait.ErrWaitTimeout) {
			klog.Errorf("TNF setup: backoff exhausted, pause %v then retry (last error: %v)", tnfJobControllerSetupCheckpointInterval, lastSetupErr)
			msg = fmt.Sprintf("TNF setup: backoff exhausted; pauses %v then retries (last error: %v)", tnfJobControllerSetupCheckpointInterval, lastSetupErr)
		} else {
			klog.Errorf("TNF setup: unexpected backoff error: %v", err)
		}

		_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "TNFJobControllersDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SetupFailed",
			Message: msg,
		}))
		if updateErr != nil {
			klog.Errorf("failed to update operator status to degraded: %v", updateErr)
		}

		if !waitTnfSetupCheckpoint(ctx) {
			return
		}
	}
}

func waitTnfSetupCheckpoint(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		klog.Infof("stopping TNF job controller setup: %v", ctx.Err())
		return false
	case <-time.After(tnfJobControllerSetupCheckpointInterval):
		return true
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
	if err := waitForTnfControlPlaneReadyAndStableRevision(ctx, kubeClient, operatorClient, nodeList); err != nil {
		return fmt.Errorf("TNF pre-job gates: %w", err)
	}
	klog.Infof("TNF pre-job gates passed; creating job controllers")

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

// waitForTnfControlPlaneReadyAndStableRevision polls 10s/10m until nodeList are Ready with control-plane labels and IsRevisionStable (OCPBUGS-84695).
func waitForTnfControlPlaneReadyAndStableRevision(ctx context.Context, kubeClient kubernetes.Interface, operatorClient v1helpers.StaticPodOperatorClient, nodeList []*corev1.Node) error {
	if len(nodeList) != 2 {
		return fmt.Errorf("TNF gates: want 2 nodes, have %d", len(nodeList))
	}
	if nodeList[0] == nil || nodeList[1] == nil {
		return fmt.Errorf("TNF gates: nil node in list")
	}
	const interval = 10 * time.Second
	const timeout = 10 * time.Minute
	klog.Infof("TNF gates: poll %v timeout %v for %q %q", interval, timeout, nodeList[0].Name, nodeList[1].Name)
	return wait.PollUntilContextTimeout(ctx, interval, timeout, false, func(ctx context.Context) (bool, error) {
		for _, ref := range nodeList {
			n, err := kubeClient.CoreV1().Nodes().Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					klog.V(4).Infof("TNF gate: node %q missing", ref.Name)
					return false, nil
				}
				return false, err
			}
			if !hasControlPlaneRoleLabels(n) {
				klog.V(4).Infof("TNF gate: node %q lacks control-plane role labels", ref.Name)
				return false, nil
			}
			ready := false
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready = true
					break
				}
			}
			if !ready {
				klog.V(4).Infof("TNF gate: node %q not Ready", ref.Name)
				return false, nil
			}
		}

		stable, err := ceohelpers.IsRevisionStable(operatorClient)
		if err != nil {
			klog.Errorf("IsRevisionStable: %v", err)
			return false, err
		}
		if !stable {
			klog.V(4).Infof("TNF gate: etcd revision not stable")
			return false, nil
		}

		return true, nil
	})
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

func runExternalEtcdSupportController(ctx context.Context,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	envVarGetter etcdenvvar.EnvVar,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	etcdInformer operatorv1informers.EtcdInformer,
	kubeClient kubernetes.Interface) {

	klog.Infof("starting external etcd support controller")
	externalEtcdSupportController := externaletcdsupportcontroller.NewExternalEtcdEnablerController(
		operatorClient,
		os.Getenv("IMAGE"),
		os.Getenv("OPERATOR_IMAGE"),
		envVarGetter,
		kubeInformersForNamespaces.InformersFor("openshift-etcd"),
		kubeInformersForNamespaces,
		infrastructureInformer,
		networkInformer,
		controlPlaneNodeInformer,
		etcdInformer,
		kubeClient,
		controllerContext.EventRecorder,
	)
	go externalEtcdSupportController.Run(ctx, 1)
}

func runTnfResourceController(ctx context.Context, controllerContext *controllercmd.ControllerContext, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface, operatorClient v1helpers.StaticPodOperatorClient, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces) {
	klog.Infof("starting Two Node Fencing static resources controller")
	tnfResourceController := staticresourcecontroller.NewStaticResourceController(
		"TnfStaticResources",
		bindata.Asset,
		[]string{
			"tnfdeployment/sa.yaml",
			"tnfdeployment/role.yaml",
			"tnfdeployment/role-binding.yaml",
			"tnfdeployment/clusterrole.yaml",
			"tnfdeployment/clusterrole-binding.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient).WithDynamicClient(dynamicClient),
		operatorClient,
		controllerContext.EventRecorder,
	).WithIgnoreNotFoundOnCreate().AddKubeInformers(kubeInformersForNamespaces)
	go tnfResourceController.Run(ctx, 1)
}

func runJobController(ctx context.Context, jobType tools.JobType, nodeName *string, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	nodeNameForLogs := "n/a"
	if nodeName != nil {
		nodeNameForLogs = *nodeName
	}
	klog.Infof("starting Two Node Fencing job controller for command %q on node %q", jobType.GetSubCommand(), nodeNameForLogs)
	tnfJobController := jobs.NewJobController(
		jobType.GetJobName(nodeName),
		bindata.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		conditions,
		[]factory.Informer{},
		[]jobs.JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				if nodeName != nil {
					job.Spec.Template.Spec.NodeName = *nodeName
				}
				job.SetName(jobType.GetJobName(nodeName))
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()
				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func handleFencingSecretChange(ctx context.Context, operatorClient v1helpers.StaticPodOperatorClient, client kubernetes.Interface, oldObj, obj interface{}) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		klog.Warningf("failed to convert added / modified / deleted object to Secret %+v", obj)
		return
	}
	if !tools.IsFencingSecret(secret.GetName()) {
		// nothing to do
		return
	}

	if oldObj != nil {
		oldSecret, ok := oldObj.(*corev1.Secret)
		if !ok {
			klog.Warningf("failed to convert old object to Secret %+v", oldObj)
		}
		// check if data changed
		changed := false
		if len(oldSecret.Data) != len(secret.Data) {
			changed = true
		} else {
			for key, oldValue := range oldSecret.Data {
				newValue, exists := secret.Data[key]
				if !exists || !bytes.Equal(oldValue, newValue) {
					changed = true
					break
				}
			}
		}
		if !changed {
			return
		}
		klog.Infof("handling modified fencing secret %s", secret.GetName())
	} else {
		klog.Infof("handling added or deleted fencing secret %s", secret.GetName())
	}

	// Defer fencing job restart from secrets until external etcd transition completes (OCPBUGS-84695).
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, operatorClient)
	if err != nil {
		klog.Errorf("failed to check external etcd transition for fencing secret handler: %v", err)
		return
	}
	if !transitionComplete {
		klog.V(4).Infof("skipping fencing job restart from fencing secret change until %s is True",
			etcd.OperatorConditionExternalEtcdHasCompletedTransition)
		return
	}

	// check if fencing update is triggered already
	fencingUpdateMutex.Lock()
	if fencingUpdateTriggered {
		klog.Infof("fencing update triggered already, skipping recreation of fencing job for secret %s", secret.GetName())
		fencingUpdateMutex.Unlock()
		return
	}
	// block further updates
	fencingUpdateTriggered = true
	fencingUpdateMutex.Unlock()

	// reset when done
	defer func() {
		fencingUpdateMutex.Lock()
		fencingUpdateTriggered = false
		fencingUpdateMutex.Unlock()
	}()

	// we need to recreate the fencing job if it exists, but don't interrupt running jobs
	klog.Infof("recreating fencing job in case it exists already, and when it's done")

	fencingJobName := tools.JobTypeFencing.GetJobName(nil)
	jobFound := false

	// helper func for waiting for a running job
	// finished = Complete, Failed, or not found
	isFencingJobFinished := func(context.Context) (finished bool, returnErr error) {
		var err error
		jobFound = false
		job, err := client.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, fencingJobName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			klog.Errorf("failed to get fencing job, will retry: %v", err)
			return false, nil
		}
		jobFound = true
		if tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) || tools.IsConditionTrue(job.Status.Conditions, batchv1.JobFailed) {
			return true, nil
		}
		klog.Infof("fencing job still running, skipping recreation for now, will retry")
		return false, nil
	}

	// wait as long as the fencing job waits as well, plus some execution time
	err = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.FencingJobCompletedTimeout, true, isFencingJobFinished)
	if err != nil {
		// if we set timeouts right, this should not happen...
		klog.Errorf("timed out waiting for fencing job to complete: %v", err)
		return
	}

	if !jobFound {
		klog.Errorf("fencing job not found, nothing to do")
		return
	}

	klog.Info("deleting fencing job for recreation")
	err = client.BatchV1().Jobs(operatorclient.TargetNamespace).Delete(ctx, fencingJobName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		// TODO how to trigger a retry here...
		klog.Errorf("failed to delete fencing job: %v", err)
	}
}

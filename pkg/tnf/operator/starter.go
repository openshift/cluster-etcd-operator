package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"

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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/externaletcdsupportcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

var (
	// fencingUpdateTriggered is set to true when a fencing update is already triggered
	fencingUpdateTriggered bool
	// fencingUpdateMutex is used to make usage of fencingUpdateTriggered thread safe
	fencingUpdateMutex sync.Mutex
)

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

	// we need node names for assigning auth and after-setup jobs to specific nodes
	controlPlaneNodeLister := corev1listers.NewNodeLister(controlPlaneNodeInformer.GetIndexer())
	klog.Infof("watching for nodes...")
	_, err = controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert added object to Node %+v", obj)
				return
			}

			// ignore nodes which are not ready yet
			if !tools.IsNodeReady(node) {
				klog.Infof("added node %s is not ready yet, skipping handling", node.GetName())
				return
			}

			// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
			// to not block the event handler
			klog.Infof("node added and ready: %s", node.GetName())
			go handleNodesWithRetry(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, oldOk := oldObj.(*corev1.Node)
			newNode, newOk := newObj.(*corev1.Node)
			if !oldOk || !newOk {
				klog.Warningf("failed to convert updated object to Node, old=%+v, new=%+v", oldObj, newObj)
				return
			}

			// only handle if node transitioned from not ready to ready
			oldReady := tools.IsNodeReady(oldNode)
			newReady := tools.IsNodeReady(newNode)
			if !oldReady && newReady {
				klog.Infof("node %s transitioned to ready state", newNode.GetName())
				// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
				// to not block the event handler
				go handleNodesWithRetry(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert deleted object to Node %+v", obj)
				return
			}
			klog.Infof("node deleted: %s", node.GetName())

			// always handle node deletion
			// this potentially needs some time when we wait for etcd bootstrap to complete, so run it in a goroutine,
			// to not block the event handler
			go handleNodesWithRetry(controllerContext, controlPlaneNodeLister, ctx, operatorClient, kubeClient, kubeInformersForNamespaces, etcdInformer)
		},
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
			handleFencingSecretChange(ctx, kubeClient, nil, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			handleFencingSecretChange(ctx, kubeClient, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			handleFencingSecretChange(ctx, kubeClient, nil, obj)
		},
	})
	if err != nil {
		klog.Errorf("failed to add eventhandler to secrets informer: %v", err)
		return false, err
	}

	return true, nil
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

func handleFencingSecretChange(ctx context.Context, client kubernetes.Interface, oldObj, obj interface{}) {
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
	err := wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.FencingJobCompletedTimeout, true, isFencingJobFinished)
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

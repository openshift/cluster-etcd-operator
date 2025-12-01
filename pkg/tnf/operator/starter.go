package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
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
			go handleFencingSecretChange(ctx, nil, obj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			go handleFencingSecretChange(ctx, oldObj, newObj, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces)
		},
		DeleteFunc: func(obj interface{}) {
			// nothing to do
			// removing orphaned fence devices is handled by update-setup job
			// when handling node replacements
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

func handleFencingSecretChange(
	ctx context.Context,
	oldObj, obj interface{},
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
) {

	// obj can be nil, always restart fencing job in that case
	if obj != nil {
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
	}

	err := jobs.RestartJobOrRunController(ctx, tools.JobTypeFencing, nil, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, jobs.DefaultConditions, tools.FencingJobCompletedTimeout)
	if err != nil {
		klog.Errorf("failed to restart fencing job: %v", err)
		return
	}
}

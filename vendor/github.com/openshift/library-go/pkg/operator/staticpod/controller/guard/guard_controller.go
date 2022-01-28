package guard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	policyclientv1 "k8s.io/client-go/kubernetes/typed/policy/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	policylisterv1 "k8s.io/client-go/listers/policy/v1"
	"k8s.io/klog/v2"

	configv1 "github.com/openshift/api/config/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/guard/bindata"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
)

// GuardController is a controller that watches amount of static pods on master nodes and
// renders guard pods with a pdb to keep maxUnavailable to be at most 1
type GuardController struct {
	targetNamespace, podResourcePrefix string
	operatorName                       string
	readyzPort                         string
	operandPodLabelSelector            labels.Selector

	nodeLister corelisterv1.NodeLister
	podLister  corelisterv1.PodLister
	podGetter  corev1client.PodsGetter
	pdbGetter  policyclientv1.PodDisruptionBudgetsGetter
	pdbLister  policylisterv1.PodDisruptionBudgetLister

	// installerPodImageFn returns the image name for the installer pod
	installerPodImageFn   func() string
	createConditionalFunc func() (bool, bool, error)
}

func NewGuardController(
	targetNamespace string,
	operandPodLabelSelector labels.Selector,
	podResourcePrefix string,
	operatorName string,
	readyzPort string,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	kubeInformersClusterScoped informers.SharedInformerFactory,
	operatorClient operatorv1helpers.StaticPodOperatorClient,
	podGetter corev1client.PodsGetter,
	pdbGetter policyclientv1.PodDisruptionBudgetsGetter,
	eventRecorder events.Recorder,
	createConditionalFunc func() (bool, bool, error),
) (factory.Controller, error) {
	if operandPodLabelSelector == nil {
		return nil, fmt.Errorf("GuardController: missing required operandPodLabelSelector")
	}
	if operandPodLabelSelector.Empty() {
		return nil, fmt.Errorf("GuardController: operandPodLabelSelector cannot be empty")
	}

	c := &GuardController{
		targetNamespace:         targetNamespace,
		operandPodLabelSelector: operandPodLabelSelector,
		podResourcePrefix:       podResourcePrefix,
		operatorName:            operatorName,
		readyzPort:              readyzPort,
		nodeLister:              kubeInformersClusterScoped.Core().V1().Nodes().Lister(),
		podLister:               kubeInformersForTargetNamespace.Core().V1().Pods().Lister(),
		podGetter:               podGetter,
		pdbGetter:               pdbGetter,
		pdbLister:               kubeInformersForTargetNamespace.Policy().V1().PodDisruptionBudgets().Lister(),
		installerPodImageFn:     getInstallerPodImageFromEnv,
		createConditionalFunc:   createConditionalFunc,
	}

	return factory.New().WithInformers(
		kubeInformersForTargetNamespace.Core().V1().Pods().Informer(),
		kubeInformersClusterScoped.Core().V1().Nodes().Informer(),
	).WithSync(c.sync).WithSyncDegradedOnError(operatorClient).ToController("GuardController", eventRecorder), nil
}

func getInstallerPodImageFromEnv() string {
	return os.Getenv("OPERATOR_IMAGE")
}

func getGuardPodName(prefix, nodeName string) string {
	return fmt.Sprintf("%s-guard-%s", prefix, nodeName)
}

func getGuardPDBName(prefix string) string {
	return fmt.Sprintf("%s-guard-pdb", prefix)
}

func nodeConditionFinder(status *corev1.NodeStatus, condType corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}

	return nil
}

func nodeHasUnschedulableTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule && taint.Key == corev1.TaintNodeUnschedulable {
			return true
		}
	}
	return false
}

func (c *GuardController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(5).Info("Syncing guards")

	if c.createConditionalFunc == nil {
		return fmt.Errorf("createConditionalFunc not set")
	}

	shouldCreate, precheckSucceeded, err := c.createConditionalFunc()
	if err != nil {
		return fmt.Errorf("createConditionalFunc returns an error: %v", err)
	}

	if !precheckSucceeded {
		klog.V(4).Infof("create conditional precheck did not succeed, skipping")
		return nil
	}

	errs := []error{}
	if !shouldCreate {
		pdb := resourceread.ReadPodDisruptionBudgetV1OrDie(bindata.MustAsset(filepath.Join("pkg/operator/staticpod/controller/guard", "manifests/pdb.yaml")))
		pdb.ObjectMeta.Name = getGuardPDBName(c.podResourcePrefix)
		pdb.ObjectMeta.Namespace = c.targetNamespace

		// List the pdb from the cache in case it does not exist and there's nothing to delete
		// so no Delete request is executed.
		pdbs, err := c.pdbLister.PodDisruptionBudgets(c.targetNamespace).List(labels.Everything())
		if err != nil {
			klog.Errorf("Unable to list PodDisruptionBudgets: %v", err)
			return err
		}

		for _, pdbItem := range pdbs {
			if pdbItem.Name == pdb.Name {
				_, _, err := resourceapply.DeletePodDisruptionBudget(ctx, c.pdbGetter, syncCtx.Recorder(), pdb)
				if err != nil {
					klog.Errorf("Unable to delete PodDisruptionBudget: %v", err)
					errs = append(errs, err)
				}
				break
			}
		}

		pods, err := c.podLister.Pods(c.targetNamespace).List(labels.SelectorFromSet(labels.Set{"app": "guard"}))
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, pod := range pods {
				_, _, err = resourceapply.DeletePod(ctx, c.podGetter, syncCtx.Recorder(), pod)
				if err != nil {
					klog.Errorf("Unable to delete Pod: %v", err)
					errs = append(errs, err)
				}
			}
		}
	} else {
		selector, err := labels.NewRequirement("node-role.kubernetes.io/master", selection.Equals, []string{""})
		if err != nil {
			panic(err)
		}
		nodes, err := c.nodeLister.List(labels.NewSelector().Add(*selector))
		if err != nil {
			return err
		}

		pods, err := c.podLister.Pods(c.targetNamespace).List(c.operandPodLabelSelector)
		if err != nil {
			return err
		}

		klog.V(5).Infof("Rendering guard pdb")

		pdb := resourceread.ReadPodDisruptionBudgetV1OrDie(bindata.MustAsset(filepath.Join("pkg/operator/staticpod/controller/guard", "manifests/pdb.yaml")))
		pdb.ObjectMeta.Name = getGuardPDBName(c.podResourcePrefix)
		pdb.ObjectMeta.Namespace = c.targetNamespace
		if len(nodes) > 1 {
			minAvailable := intstr.FromInt(len(nodes) - 1)
			pdb.Spec.MinAvailable = &minAvailable
		}

		pdbObj, err := c.pdbLister.PodDisruptionBudgets(pdb.Namespace).Get(pdb.Name)
		if err == nil {
			if pdbObj.Spec.MinAvailable != pdb.Spec.MinAvailable {
				_, _, err = resourceapply.ApplyPodDisruptionBudget(ctx, c.pdbGetter, syncCtx.Recorder(), pdb)
				if err != nil {
					klog.Errorf("Unable to apply PodDisruptionBudget changes: %v", err)
					return fmt.Errorf("Unable to apply PodDisruptionBudget changes: %v", err)
				}
			}
		} else if errors.IsNotFound(err) {
			_, _, err = resourceapply.ApplyPodDisruptionBudget(ctx, c.pdbGetter, syncCtx.Recorder(), pdb)
			if err != nil {
				klog.Errorf("Unable to create PodDisruptionBudget: %v", err)
				return fmt.Errorf("Unable to create PodDisruptionBudget: %v", err)
			}
		} else {
			klog.Errorf("Unable to get PodDisruptionBudget: %v", err)
			return err
		}

		operands := map[string]*corev1.Pod{}
		for _, pod := range pods {
			operands[pod.Spec.NodeName] = pod
		}

		for _, node := range nodes {
			// Check whether the node is schedulable
			if nodeHasUnschedulableTaint(node) {
				klog.Infof("Node %v not schedulable, skipping reconciling the guard pod", node.Name)
				continue
			}

			if _, exists := operands[node.Name]; !exists {
				// If the operand does not exist and the node is not ready, wait until the node becomes ready
				nodeReadyCondition := nodeConditionFinder(&node.Status, corev1.NodeReady)
				// If a "Ready" condition is not found, that node should be deemed as not Ready by default.
				if nodeReadyCondition == nil || nodeReadyCondition.Status != corev1.ConditionTrue {
					klog.Infof("Node %v not ready, skipping reconciling the guard pod", node.Name)
					continue
				}

				klog.Errorf("Missing operand on node %v", node.Name)
				errs = append(errs, fmt.Errorf("Missing operand on node %v", node.Name))
				continue
			}

			if operands[node.Name].Status.PodIP == "" {
				klog.Errorf("Missing PodIP in operand %v on node %v", operands[node.Name].Name, node.Name)
				errs = append(errs, fmt.Errorf("Missing PodIP in operand %v on node %v", operands[node.Name].Name, node.Name))
				continue
			}

			klog.V(5).Infof("Rendering guard pod for operand %v on node %v", operands[node.Name].Name, node.Name)

			pod := resourceread.ReadPodV1OrDie(bindata.MustAsset(filepath.Join("pkg/operator/staticpod/controller/guard", "manifests/guard-pod.yaml")))

			pod.ObjectMeta.Name = getGuardPodName(c.podResourcePrefix, node.Name)
			pod.ObjectMeta.Namespace = c.targetNamespace
			pod.Spec.NodeName = node.Name
			pod.Spec.Containers[0].Image = c.installerPodImageFn()
			pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Host = operands[node.Name].Status.PodIP
			// The readyz port as string type is expected to be convertible into int!!!
			readyzPort, err := strconv.Atoi(c.readyzPort)
			if err != nil {
				panic(err)
			}
			pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port = intstr.FromInt(readyzPort)

			actual, err := c.podGetter.Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if err == nil {
				// Delete the pod so it can be re-created. ApplyPod only updates the metadata part of the manifests, ignores the rest
				delete := false
				if actual.Spec.Containers[0].Image != pod.Spec.Containers[0].Image {
					klog.V(5).Infof("Guard Image changed, deleting %v so the guard can be re-created", pod.Name)
					delete = true
				}
				if actual.Spec.Containers[0].ReadinessProbe.HTTPGet.Host != pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Host {
					klog.V(5).Infof("Operand PodIP changed, deleting %v so the guard can be re-created", pod.Name)
					delete = true
				}
				if delete {
					_, _, err = resourceapply.DeletePod(ctx, c.podGetter, syncCtx.Recorder(), pod)
					if err != nil {
						klog.Errorf("Unable to delete Pod for immidiate re-creation: %v", err)
						errs = append(errs, fmt.Errorf("Unable to delete Pod for immidiate re-creation: %v", err))
						continue
					}
				}
			} else if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
				continue
			}

			_, _, err = resourceapply.ApplyPod(ctx, c.podGetter, syncCtx.Recorder(), pod)
			if err != nil {
				klog.Errorf("Unable to apply pod %v changes: %v", pod.Name, err)
				errs = append(errs, fmt.Errorf("Unable to apply pod %v changes: %v", pod.Name, err))
			}
		}
	}

	return utilerrors.NewAggregate(errs)
}

// IsSNOCheckFnc creates a function that checks if the topology is SNO
// In case the err is nil, precheckSucceeded signifies whether the isSNO is valid.
// If precheckSucceeded is false, the isSNO return value does not reflect the cluster topology
// and defaults to the bool default value.
func IsSNOCheckFnc(infraInformer configv1informers.InfrastructureInformer) func() (isSNO, precheckSucceeded bool, err error) {
	return func() (isSNO, precheckSucceeded bool, err error) {
		if !infraInformer.Informer().HasSynced() {
			// Do not return transient error
			return false, false, nil
		}
		infraData, err := infraInformer.Lister().Get("cluster")
		if err != nil {
			return false, true, fmt.Errorf("Unable to list infrastructures.config.openshift.io/cluster object, unable to determine topology mode")
		}
		if infraData.Status.ControlPlaneTopology == "" {
			return false, true, fmt.Errorf("ControlPlaneTopology was not set, unable to determine topology mode")
		}

		return infraData.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode, true, nil
	}
}

package machinedeletionhooks

import (
	"context"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"time"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	machinev1beta1client "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1beta1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

type machineDeletionHooksController struct {
	etcdClient                            etcdcli.EtcdClient
	machineClient                         machinev1beta1client.MachineInterface
	masterMachineLister                   machinelistersv1beta1.MachineLister
	configMapListerForKubeSystemNamespace corev1listers.ConfigMapNamespaceLister
	// machineAPIChecker determines if the precondition for this controller is met,
	// this controller can be run only on a cluster that exposes a functional Machine API
	machineAPIChecker     ceohelpers.MachineAPIChecker
	masterMachineSelector labels.Selector
}

// NewMachineDeletionHooksController reconciles machine hooks for master machines
//
// note:
//  The Machine Deletion Hook this controller reconciles is a mechanism within the Machine API that allow this operator to hold up removal of a machine
//  until a replacement member has been promoted to a voting member.
func NewMachineDeletionHooksController(
	operatorClient operatorv1helpers.OperatorClient,
	machineClient machinev1beta1client.MachineInterface,
	etcdClient etcdcli.EtcdClient,
	machineAPIChecker ceohelpers.MachineAPIChecker,
	masterMachineSelector labels.Selector,
	kubeInformersForNamespaces operatorv1helpers.KubeInformersForNamespaces,
	masterMachineInformer cache.SharedIndexInformer,
	eventRecorder events.Recorder) factory.Controller {
	c := &machineDeletionHooksController{
		machineClient:                         machineClient,
		etcdClient:                            etcdClient,
		machineAPIChecker:                     machineAPIChecker,
		masterMachineSelector:                 masterMachineSelector,
		masterMachineLister:                   machinelistersv1beta1.NewMachineLister(masterMachineInformer.GetIndexer()),
		configMapListerForKubeSystemNamespace: kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Lister().ConfigMaps("kube-system"),
	}
	return factory.New().
		WithSync(c.sync).
		WithSyncDegradedOnError(operatorClient).
		WithInformers(
			masterMachineInformer,
			kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Informer(),
		).
		ResyncEvery(1*time.Minute). // an arbitrary number, not too high not too low
		ToController("MachineDeletionHooksController", eventRecorder.WithComponentSuffix("cluster-machine-deletion-hooks-controller"))
}

func (c *machineDeletionHooksController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	// stop if the machine API is not functional
	if isFunctional, err := c.machineAPIChecker.IsFunctional(); err != nil {
		return err
	} else if !isFunctional {
		return nil
	}
	var errs []error
	if err := c.addDeletionHookToMasterMachines(ctx); err != nil {
		errs = append(errs, err)
	}
	// attempt to remove hooks from machines pending deletion and without running etcd member
	if err := c.attemptToDeleteMachineDeletionHook(ctx, syncCtx.Recorder()); err != nil {
		errs = append(errs, err)
	}
	return kerrors.NewAggregate(errs)
}

// it will only remove the hook once it has identified that the Machine resource is being deleted and a replacement member has been created.
func (c *machineDeletionHooksController) attemptToDeleteMachineDeletionHook(ctx context.Context, recorder events.Recorder) error {
	desiredControlPlaneReplicasCount, err := ceohelpers.ReadDesiredControlPlaneReplicasCount(c.configMapListerForKubeSystemNamespace)
	if err != nil {
		return err
	}
	masterMachines, err := c.masterMachineLister.List(c.masterMachineSelector)
	if err != nil {
		return err
	}
	// machines with the deletion hooks are the ones that host etcd members
	memberMachines := ceohelpers.FilterMachinesWithMachineDeletionHook(masterMachines)

	// if the number of master machines is > controlPlane.replicas based on data from the cache
	if len(memberMachines) <= desiredControlPlaneReplicasCount {
		klog.V(4).Infof("haven't found a replacement machine, the number of desired control plane replicas: %d must be greater than the current number of machines that host an etcd member: %d", desiredControlPlaneReplicasCount, len(memberMachines))
		return nil
	}

	// if at least one member machine is pending deletion
	memberMachinesPendingDeletion := ceohelpers.FilterMachinesPendingDeletion(memberMachines)
	if len(memberMachinesPendingDeletion) == 0 {
		klog.V(4).Infof("currently, we don't have a machine pending deletion")
		return nil
	}

	// get the members, issue a live call instead of reading from the config map
	// because we need to take into account learners
	members, err := c.etcdClient.MemberList(ctx)
	if err != nil {
		return err
	}
	currentMemberIPListSet := sets.NewString()
	for _, member := range members {
		memberIP, err := ceohelpers.MemberToNodeInternalIP(member)
		if err != nil {
			return err
		}
		currentMemberIPListSet.Insert(memberIP)
	}

	// find a machine that doesn't have a member
	for memberMachinePendingDeletionIP, memberMachinePendingDeletion := range indexMachinesByNodeInternalIP(memberMachinesPendingDeletion) {
		if currentMemberIPListSet.Has(memberMachinePendingDeletionIP) {
			// Ignore machines that still have a member present
			continue
		}
		memberMachinePendingDeletionCopy := memberMachinePendingDeletion.DeepCopy()
		removeMachineDeletionHook(memberMachinePendingDeletionCopy)
		if _, err := c.machineClient.Update(ctx, memberMachinePendingDeletionCopy, metav1.UpdateOptions{}); err == nil {
			klog.V(2).Infof("successfully removed the deletion hook from machine %v", memberMachinePendingDeletion.Name)
			recorder.Eventf("MachineDeletionHook", "successfully removed the deletion hook from machine %v as it doesn't have a member", memberMachinePendingDeletion.Name)
		}
		return err
	}
	return nil
}

// addDeletionHookToMasterMachines adds the Machine Deletion Hook to master machines
func (c *machineDeletionHooksController) addDeletionHookToMasterMachines(ctx context.Context) error {
	masterMachines, err := c.masterMachineLister.List(c.masterMachineSelector)
	if err != nil {
		return err
	}
	for _, masterMachine := range masterMachines {
		if masterMachine.DeletionTimestamp != nil {
			continue
		}
		if hasCompleteMachineDeletionHook(masterMachine) {
			continue
		}
		masterMachineCopy := masterMachine.DeepCopy()
		if hasPartialMachineDeletionHook(masterMachineCopy) {
			repairPartialMachineDeletionHook(masterMachineCopy)
		} else {
			addPreDrainHook(masterMachineCopy)
		}
		if _, err := c.machineClient.Update(ctx, masterMachineCopy, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func hasCompleteMachineDeletionHook(machine *machinev1beta1.Machine) bool {
	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == ceohelpers.MachineDeletionHookName && hook.Owner == ceohelpers.MachineDeletionHookOwner {
			return true
		}
	}
	return false
}

func hasPartialMachineDeletionHook(machine *machinev1beta1.Machine) bool {
	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == ceohelpers.MachineDeletionHookName && hook.Owner != ceohelpers.MachineDeletionHookOwner {
			return true
		}
	}
	return false
}

func repairPartialMachineDeletionHook(machine *machinev1beta1.Machine) {
	var newPreDrainHooks []machinev1beta1.LifecycleHook

	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == ceohelpers.MachineDeletionHookName {
			continue
		}
		newPreDrainHooks = append(newPreDrainHooks, hook)
	}
	machine.Spec.LifecycleHooks.PreDrain = newPreDrainHooks
	addPreDrainHook(machine)
}

func addPreDrainHook(machine *machinev1beta1.Machine) {
	machine.Spec.LifecycleHooks.PreDrain = append(machine.Spec.LifecycleHooks.PreDrain, machinev1beta1.LifecycleHook{Name: ceohelpers.MachineDeletionHookName, Owner: ceohelpers.MachineDeletionHookOwner})
}

func removeMachineDeletionHook(machine *machinev1beta1.Machine) {
	var newPreDrainHooks []machinev1beta1.LifecycleHook
	for _, hook := range machine.Spec.LifecycleHooks.PreDrain {
		if hook.Name == ceohelpers.MachineDeletionHookName {
			continue
		}
		newPreDrainHooks = append(newPreDrainHooks, hook)
	}
	machine.Spec.LifecycleHooks.PreDrain = newPreDrainHooks
}

func indexMachinesByNodeInternalIP(machines []*machinev1beta1.Machine) map[string]*machinev1beta1.Machine {
	index := map[string]*machinev1beta1.Machine{}
	for _, machine := range machines {
		for _, addr := range machine.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				index[addr.Address] = machine
				break
			}
		}
	}
	return index
}

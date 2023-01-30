package missingstaticpodcontroller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/staticpod/internal"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	timeoutMultiNode  = time.Second * 150
	timeoutSingleNode = time.Second * 180
)

type snoDeploymentFunc func() (bool, bool, error)

type missingStaticPodController struct {
	operatorClient                    v1helpers.StaticPodOperatorClient
	podListerForTargetNamespace       corelisterv1.PodNamespaceLister
	configMapListerForTargetNamespace corelisterv1.ConfigMapNamespaceLister
	targetNamespace                   string
	staticPodName                     string
	operandName                       string

	lastEventEmissionPerNode lastEventEmissionPerNode

	isSNODeployment snoDeploymentFunc
}

type lastEventEmissionPerNode map[string]struct {
	revision  int
	timestamp time.Time
}

// New checks the latest static pods pods and uses the installer to get the
// latest revision.  If the installer pod was successful, and if it has been
// longer than the terminationGracePeriodSeconds+maxTimeout(value depends on
// single node or multi node deployment) seconds since the installer pod
// completed successfully, and if the static pod is not at the correct revision
// , this controller will go degraded.  It will also emit an event for detection
// in CI.
func New(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
	targetNamespace string,
	staticPodName string,
	operandName string,
	infraInformer configv1informers.InfrastructureInformer,
) factory.Controller {
	c := &missingStaticPodController{
		operatorClient:                    operatorClient,
		podListerForTargetNamespace:       kubeInformersForTargetNamespace.Core().V1().Pods().Lister().Pods(targetNamespace),
		configMapListerForTargetNamespace: kubeInformersForTargetNamespace.Core().V1().ConfigMaps().Lister().ConfigMaps(targetNamespace),
		targetNamespace:                   targetNamespace,
		staticPodName:                     staticPodName,
		operandName:                       operandName,
		lastEventEmissionPerNode:          make(lastEventEmissionPerNode),
		isSNODeployment:                   common.NewIsSingleNodePlatformFn(infraInformer),
	}

	return factory.New().
		ResyncEvery(time.Minute).
		WithInformers(
			operatorClient.Informer(),
			kubeInformersForTargetNamespace.Core().V1().Pods().Informer(),
			infraInformer.Informer(),
		).
		WithBareInformers(
			kubeInformersForTargetNamespace.Core().V1().ConfigMaps().Informer(),
		).
		WithSync(c.sync).
		WithSyncDegradedOnError(operatorClient).
		ToController("MissingStaticPodController", eventRecorder)
}

func (c *missingStaticPodController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	installerPods, err := c.podListerForTargetNamespace.List(labels.SelectorFromSet(labels.Set{"app": "installer"}))
	if err != nil {
		return err
	}

	// get the most recent installer pod for each node
	latestInstallerPodsByNode, err := getMostRecentInstallerPodByNode(installerPods)
	if err != nil {
		return err
	}

	errors := make([]string, 0, len(latestInstallerPodsByNode))
	for node, latestInstallerPodOnNode := range latestInstallerPodsByNode {
		installerPodRevision, err := internal.GetRevisionOfPod(latestInstallerPodOnNode)
		if err != nil {
			// we expect every installer pod to have a installerPodRevision in its name, unexpected error here
			return fmt.Errorf("failed to get installerPodRevision for installer pod %q - %w", latestInstallerPodOnNode.Name, err)
		}

		finishedAt, ok := installerPodFinishedAt(latestInstallerPodOnNode)
		if !ok {
			// either it's in the process of running, or it ran into an error
			continue
		}

		gracePeriod, err := c.getStaticPodTerminationGracePeriodSecondsForRevision(installerPodRevision)
		if err != nil {
			return err
		}

		isSNO, preconditionFulfilled, err := c.isSNODeployment()
		if err != nil {
			return err
		}
		maxTimeout := timeoutMultiNode
		// In practice the preconditionFulfilled should always be true because the controller
		// waits for the infra informer. If this is not the case and we get a failure (it only
		// fails when the informer is not synced), we choose the conservative approach of
		// selecting the longer timeout in case we dont know if we are running in SNO.
		if !preconditionFulfilled || isSNO {
			maxTimeout = timeoutSingleNode
		}

		threshold := gracePeriod + maxTimeout

		if time.Since(finishedAt) > threshold {
			// if we are here then:
			//  a: the latest installer pod successfully completed at finishedAt
			//  b: it has been more than 'terminationGracePeriodSeconds' + maxTimeout
			//     since the installer pod has completed at finishedAt
			//
			// now check to see if we have a mirror pod on this node
			staticPodRevisionOnThisNode, err := c.getStaticPodCurrentRevisionForNode(node)
			if err != nil && !apierrors.IsNotFound(err) {
				// we expect every static pod to have a valid revision
				return fmt.Errorf("failed to get a revision for the static pod %q - %w", mirrorStaticPodNameForNode(c.staticPodName, node), err)
			}
			if staticPodRevisionOnThisNode < installerPodRevision {
				// if we are here then:
				//  c. the static pod is not at correct installerPodRevision yet
				//
				// in addition to that all the previous conditions are also true
				// so, we should produce an event but only when an event is for a particular node and:
				//  a: this is the first time we see a failure
				//  b: this is a failure for new revision
				//  c: the previously reported event was at least 30 min ago
				lastEventEmission, found := c.lastEventEmissionPerNode[node]
				if !found || installerPodRevision != lastEventEmission.revision || time.Since(lastEventEmission.timestamp) > 30*time.Minute {
					syncCtx.Recorder().Eventf("MissingStaticPod", "static pod lifecycle failure - static pod: %q in namespace: %q for revision: %d on node: %q didn't show up, waited: %v",
						c.staticPodName, c.targetNamespace, installerPodRevision, node, threshold)

					lastEventEmission.revision = installerPodRevision
					lastEventEmission.timestamp = time.Now()
					c.lastEventEmissionPerNode[node] = lastEventEmission
				}

				errors = append(errors, fmt.Sprintf("static pod lifecycle failure - static pod: %q in namespace: %q for revision: %d on node: %q didn't show up, waited: %v",
					c.staticPodName, c.targetNamespace, installerPodRevision, node, threshold))
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf(strings.Join(errors, "\n"))
	}

	return nil
}

// getStaticPodCurrentRevisionForNode reads the current revision from the static pod for the given node
// since the names are uniques and we know how to construct the final pod's name we always expect to get the desired pod
func (c *missingStaticPodController) getStaticPodCurrentRevisionForNode(nodeName string) (int, error) {
	staticPod, err := c.podListerForTargetNamespace.Get(mirrorStaticPodNameForNode(c.staticPodName, nodeName))
	if err != nil {
		return -1, err
	}
	revisionStr := staticPod.Labels["revision"]
	revision, err := strconv.Atoi(revisionStr)
	if err != nil {
		return -1, err
	}
	return revision, nil
}

// getStaticPodTerminationGracePeriodSecondsForRevision reads the static pod manifest from a configmap
// in the target namespace and returns the value of terminationGracePeriodSeconds.
// In case no value was provided for the static pod it returns a default value of 30 seconds.
func (c *missingStaticPodController) getStaticPodTerminationGracePeriodSecondsForRevision(revision int) (time.Duration, error) {
	staticPodKeyName := "pod.yaml"
	staticPodConfigMapName := fmt.Sprintf("%s-pod-%d", c.operandName, revision)

	staticPodConfigMap, err := c.configMapListerForTargetNamespace.Get(staticPodConfigMapName)
	if err != nil {
		return 0, err
	}

	rawStaticPodManifest, hasPodKey := staticPodConfigMap.Data[staticPodKeyName]
	if !hasPodKey {
		return 0, fmt.Errorf("didn't find required key %q in cm: %s/%s", staticPodKeyName, staticPodConfigMapName, c.targetNamespace)
	}

	serializedStaticPod, err := resourceread.ReadPodV1([]byte(rawStaticPodManifest))
	if err != nil {
		return 0, err
	}

	if serializedStaticPod.Spec.TerminationGracePeriodSeconds == nil {
		klog.V(6).Infof("optional field: %s.spec.terminationGracePeriodSeconds was not specified in cm: %s, returning default value of 30s", staticPodKeyName, staticPodConfigMapName)
		return 30 * time.Second, nil
	}

	return time.Duration(*serializedStaticPod.Spec.TerminationGracePeriodSeconds) * time.Second, nil
}

func getMostRecentInstallerPodByNode(pods []*corev1.Pod) (map[string]*corev1.Pod, error) {
	mostRecentInstallerPodByNode := map[string]*corev1.Pod{}
	byNodes, err := getInstallerPodsByNode(pods)
	if err != nil {
		return nil, err
	}

	for node, installerPodsOnThisNode := range byNodes {
		if len(installerPodsOnThisNode) == 0 {
			continue
		}
		sort.Sort(internal.ByRevision(installerPodsOnThisNode))
		mostRecentInstallerPodByNode[node] = installerPodsOnThisNode[len(installerPodsOnThisNode)-1]
	}

	return mostRecentInstallerPodByNode, nil
}

func getInstallerPodsByNode(pods []*corev1.Pod) (map[string][]*corev1.Pod, error) {
	byNodes := map[string][]*corev1.Pod{}
	for i := range pods {
		pod := pods[i]
		if !strings.HasPrefix(pod.Name, "installer-") {
			continue
		}

		nodeName := pod.Spec.NodeName
		if len(nodeName) == 0 {
			return nil, fmt.Errorf("node name for installer pod %q is empty", pod.Name)
		}
		byNodes[nodeName] = append(byNodes[nodeName], pod)
	}

	return byNodes, nil
}

// installerPodFinishedAt returns the 'finishedAt' time for an installer
// pod that has completed successfully
func installerPodFinishedAt(pod *corev1.Pod) (time.Time, bool) {
	statuses := pod.Status.ContainerStatuses
	if len(statuses) == 0 {
		return time.Time{}, false
	}

	// we are looking for container name "installer"
	var installerContainerStatus *corev1.ContainerStatus
	for i := range statuses {
		if statuses[i].Name == "installer" {
			installerContainerStatus = &statuses[i]
			break
		}
	}
	if installerContainerStatus == nil {
		return time.Time{}, false
	}

	terminated := installerContainerStatus.State.Terminated
	if terminated == nil || terminated.ExitCode != 0 {
		return time.Time{}, false
	}

	return terminated.FinishedAt.Time, true
}

func mirrorStaticPodNameForNode(staticPodName, nodeName string) string {
	return staticPodName + "-" + nodeName
}

package installer

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer/bindata"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/revision"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
)

const (
	manifestDir              = "pkg/operator/staticpod/controller/installer"
	manifestInstallerPodPath = "manifests/installer-pod.yaml"

	hostResourceDirDir = "/etc/kubernetes/static-pod-resources"
	hostPodManifestDir = "/etc/kubernetes/manifests"

	revisionLabel       = "revision"
	statusConfigMapName = "revision-status"
)

// InstallerController is a controller that watches the currentRevision and targetRevision fields for each node and spawn
// installer pods to update the static pods on the master nodes.
type InstallerController struct {
	targetNamespace, staticPodName string
	// configMaps is the list of configmaps that are directly copied.A different actor/controller modifies these.
	// the first element should be the configmap that contains the static pod manifest
	configMaps []revision.RevisionResource
	// secrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
	secrets []revision.RevisionResource
	// minReadySeconds is the time to wait between the completion of an operand becoming ready (all containers ready)
	// and starting the rollout onto the next node.  This avoids a problem with an external load balancer that looks like
	//  1. for some reason we have two instances, maybe a liveness check blipped on one node. it doesn't matter why
	//  2. we bring down an instance on m0 to start a new revision
	//  3. at this point we have one instance running on m1
	//  4. m0 starts up and goes ready, but the LB ready check just timed out and is waiting for X seconds
	//  5. we bring down an instance on m1 to start the new revision.
	//  6. the LB thinks all backends are down and routes randomly
	//  7. no profit.
	// setting this field to 30s can prevent the kube-apiserver from triggering the above flow on AWS.
	minReadyDuration time.Duration
	// command is the string to use for the installer pod command
	command []string

	// these are copied separately at the beginning to a fixed location
	certConfigMaps []UnrevisionedResource
	certSecrets    []UnrevisionedResource
	certDir        string

	operatorClient v1helpers.StaticPodOperatorClient

	configMapsGetter corev1client.ConfigMapsGetter
	secretsGetter    corev1client.SecretsGetter
	podsGetter       corev1client.PodsGetter
	eventRecorder    events.Recorder
	now              func() time.Time // for test plumbing

	// installerPodImageFn returns the image name for the installer pod
	installerPodImageFn func() string
	// ownerRefsFn sets the ownerrefs on the pruner pod
	ownerRefsFn func(revision int32) ([]metav1.OwnerReference, error)

	installerPodMutationFns []InstallerPodMutationFunc

	factory *factory.Factory
	clock   clock.Clock
}

// InstallerPodMutationFunc is a function that has a chance at changing the installer pod before it is created
type InstallerPodMutationFunc func(pod *corev1.Pod, nodeName string, operatorSpec *operatorv1.StaticPodOperatorSpec, revision int32) error

func (c *InstallerController) WithInstallerPodMutationFn(installerPodMutationFn InstallerPodMutationFunc) *InstallerController {
	c.installerPodMutationFns = append(c.installerPodMutationFns, installerPodMutationFn)
	return c
}

func (c *InstallerController) WithMinReadyDuration(minReadyDuration time.Duration) *InstallerController {
	c.minReadyDuration = minReadyDuration
	return c
}

func (c *InstallerController) WithCerts(certDir string, certConfigMaps, certSecrets []UnrevisionedResource) *InstallerController {
	c.certDir = certDir
	c.certConfigMaps = certConfigMaps
	c.certSecrets = certSecrets
	return c
}

// staticPodState is the status of a static pod that has been installed to a node.
type staticPodState int

const (
	// staticPodStatePending means that the installed static pod is not up yet.
	staticPodStatePending = staticPodState(iota)
	// staticPodStateReady means that the installed static pod is ready.
	staticPodStateReady
	// staticPodStateFailed means that the static pod installation of a node has failed.
	staticPodStateFailed
)

var _ factory.Controller = &InstallerController{}

type UnrevisionedResource struct {
	Name     string
	Optional bool
}

// NewInstallerController creates a new installer controller.
func NewInstallerController(
	targetNamespace, staticPodName string,
	configMaps []revision.RevisionResource,
	secrets []revision.RevisionResource,
	command []string,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	operatorClient v1helpers.StaticPodOperatorClient,
	configMapsGetter corev1client.ConfigMapsGetter,
	secretsGetter corev1client.SecretsGetter,
	podsGetter corev1client.PodsGetter,
	eventRecorder events.Recorder,
) *InstallerController {
	c := &InstallerController{
		targetNamespace: targetNamespace,
		staticPodName:   staticPodName,
		configMaps:      configMaps,
		secrets:         secrets,
		command:         command,

		operatorClient:   operatorClient,
		configMapsGetter: configMapsGetter,
		secretsGetter:    secretsGetter,
		podsGetter:       podsGetter,
		eventRecorder:    eventRecorder.WithComponentSuffix("installer-controller"),
		now:              time.Now,

		installerPodImageFn: getInstallerPodImageFromEnv,
		clock:               clock.RealClock{},
	}

	c.ownerRefsFn = c.setOwnerRefs
	c.factory = factory.New().WithInformers(operatorClient.Informer(), kubeInformersForTargetNamespace.Core().V1().Pods().Informer())

	return c
}

func (c *InstallerController) Run(ctx context.Context, workers int) {
	c.factory.WithSync(c.Sync).ToController(c.Name(), c.eventRecorder).Run(ctx, workers)
}

func (c InstallerController) Name() string {
	return "InstallerController"
}

func (c *InstallerController) getStaticPodState(ctx context.Context, nodeName string) (state staticPodState, revision, reason string, errors []string, err error) {
	pod, err := c.podsGetter.Pods(c.targetNamespace).Get(ctx, mirrorPodNameForNode(c.staticPodName, nodeName), metav1.GetOptions{})
	if err != nil {
		return staticPodStatePending, "", "", nil, err
	}
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded:
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return staticPodStateReady, pod.Labels[revisionLabel], "static pod is ready", nil, nil
			}
		}
		return staticPodStatePending, pod.Labels[revisionLabel], "static pod is not ready", nil, nil
	case corev1.PodFailed:
		return staticPodStateFailed, pod.Labels[revisionLabel], "static pod has failed", []string{pod.Status.Message}, nil
	}

	return staticPodStatePending, pod.Labels[revisionLabel], fmt.Sprintf("static pod has unknown phase: %v", pod.Status.Phase), nil, nil
}

type staticPodStateFunc func(ctx context.Context, nodeName string) (state staticPodState, revision, reason string, errors []string, err error)

// nodeToStartRevisionWith returns a node index i and guarantees for every node < i that it is
// - not updating
// - ready
// - at the revision claimed in CurrentRevision.
func nodeToStartRevisionWith(ctx context.Context, getStaticPodStateFn staticPodStateFunc, nodes []operatorv1.NodeStatus) (int, string, error) {
	if len(nodes) == 0 {
		return 0, "", fmt.Errorf("nodes array cannot be empty")
	}

	// find upgrading node as this will be the first to start new revision (to minimize number of down nodes)
	for i := range nodes {
		if nodes[i].TargetRevision != 0 {
			reason := fmt.Sprintf("node %s is progressing towards %d", nodes[i].NodeName, nodes[i].TargetRevision)
			return i, reason, nil
		}
	}
	var mostCurrent int32
	for i := range nodes {
		if nodes[i].CurrentRevision > mostCurrent {
			mostCurrent = nodes[i].CurrentRevision
		}
	}
	for i := range nodes {
		if nodes[i].LastFailedRevision > mostCurrent {
			reason := fmt.Sprintf("node %s is progressing with failed revisions", nodes[i].NodeName)
			return i, reason, nil
		}
	}

	// otherwise try to find a node that is not ready. Take the oldest one.
	oldestNotReadyRevisionNode := -1
	oldestNotReadyRevision := math.MaxInt32
	for i := range nodes {
		currNodeState := &nodes[i]
		state, runningRevision, _, _, err := getStaticPodStateFn(ctx, currNodeState.NodeName)
		if err != nil && apierrors.IsNotFound(err) {
			return i, fmt.Sprintf("node %s static pod not found", currNodeState.NodeName), nil
		}
		if err != nil {
			return 0, "", err
		}
		revisionNum, err := strconv.Atoi(runningRevision)
		if err != nil {
			reason := fmt.Sprintf("node %s has an invalid current revision %q", currNodeState.NodeName, runningRevision)
			return i, reason, nil
		}
		if state != staticPodStateReady && revisionNum < oldestNotReadyRevision {
			oldestNotReadyRevisionNode = i
			oldestNotReadyRevision = revisionNum
		}
	}
	if oldestNotReadyRevisionNode >= 0 {
		reason := fmt.Sprintf("node %s with revision %d is the oldest not ready", nodes[oldestNotReadyRevisionNode].NodeName, oldestNotReadyRevision)
		return oldestNotReadyRevisionNode, reason, nil
	}

	// find a node that has the wrong revision. Take the oldest one.
	oldestPodRevisionNode := -1
	oldestPodRevision := math.MaxInt32
	for i := range nodes {
		currNodeState := &nodes[i]
		_, runningRevision, _, _, err := getStaticPodStateFn(ctx, currNodeState.NodeName)
		if err != nil && apierrors.IsNotFound(err) {
			return i, fmt.Sprintf("node %s static pod not found", currNodeState.NodeName), nil
		}
		if err != nil {
			return 0, "", err
		}
		revisionNum, err := strconv.Atoi(runningRevision)
		if err != nil {
			reason := fmt.Sprintf("node %s has an invalid current revision %q", currNodeState.NodeName, runningRevision)
			return i, reason, nil
		}
		if revisionNum != int(currNodeState.CurrentRevision) && revisionNum < oldestPodRevision {
			oldestPodRevisionNode = i
			oldestPodRevision = revisionNum
		}
	}
	if oldestPodRevisionNode >= 0 {
		reason := fmt.Sprintf("node %s with revision %d is the oldest not matching its expected revision %d", nodes[oldestPodRevisionNode].NodeName, oldestPodRevisionNode, nodes[oldestPodRevisionNode].CurrentRevision)
		return oldestPodRevisionNode, reason, nil
	}

	// last but not least, choose the one with the older current revision. This will imply that failed installer pods will be retried.
	oldestCurrentRevisionNode := -1
	oldestCurrentRevision := int32(math.MaxInt32)
	for i := range nodes {
		currNodeState := &nodes[i]
		if currNodeState.CurrentRevision < oldestCurrentRevision {
			oldestCurrentRevisionNode = i
			oldestCurrentRevision = currNodeState.CurrentRevision
		}
	}
	if oldestCurrentRevisionNode >= 0 {
		reason := fmt.Sprintf("node %s with revision %d is the oldest", nodes[oldestCurrentRevisionNode].NodeName, oldestCurrentRevision)
		return oldestCurrentRevisionNode, reason, nil
	}

	reason := fmt.Sprintf("node %s of revision %d is no worse than any other node, but comes first", nodes[0].NodeName, oldestCurrentRevision)
	return 0, reason, nil
}

// timeToWaitBeforeInstallingNextPod determines the amount of time to delay before creating the next installer pod.
// We delay to avoid issues where the the LB doesn't observe readyz for ready pods as quickly as kubelet does.
// See godoc on minReadyDuration.
func (c *InstallerController) timeToWaitBeforeInstallingNextPod(ctx context.Context, nodeStatuses []operatorv1.NodeStatus) time.Duration {
	if c.minReadyDuration == 0 {
		return 0
	}
	// long enough that we would notice if something went really wrong.  Short enough that a customer cluster will still function
	minDurationPodHasBeenReady := 600 * time.Second
	for _, nodeStatus := range nodeStatuses {
		pod, err := c.podsGetter.Pods(c.targetNamespace).Get(ctx, mirrorPodNameForNode(c.staticPodName, nodeStatus.NodeName), metav1.GetOptions{})
		if err != nil {
			// if we have an issue getting the static pod, just don't bother delaying for minReadySeconds at all
			continue
		}
		for _, podCondition := range pod.Status.Conditions {
			if podCondition.Type != corev1.PodReady {
				continue
			}
			if podCondition.Status != corev1.ConditionTrue {
				continue
			}
			durationPodHasBeenReady := c.clock.Now().Sub(podCondition.LastTransitionTime.Time)
			if durationPodHasBeenReady < minDurationPodHasBeenReady {
				minDurationPodHasBeenReady = durationPodHasBeenReady
			}
		}
	}
	// if we've been ready longer than the minimum, don't wait
	if minDurationPodHasBeenReady > c.minReadyDuration {
		return 0
	}

	// otherwise wait the balance
	return c.minReadyDuration - minDurationPodHasBeenReady
}

// manageInstallationPods takes care of creating content for the static pods to install.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func (c *InstallerController) manageInstallationPods(ctx context.Context, operatorSpec *operatorv1.StaticPodOperatorSpec, originalOperatorStatus *operatorv1.StaticPodOperatorStatus) (bool, error) {
	operatorStatus := originalOperatorStatus.DeepCopy()

	if len(operatorStatus.NodeStatuses) == 0 {
		return false, nil
	}

	// start with node which is in worst state (instead of terminating healthy pods first)
	startNode, nodeChoiceReason, err := nodeToStartRevisionWith(ctx, c.getStaticPodState, operatorStatus.NodeStatuses)
	if err != nil {
		return true, err
	}

	// determine the amount of time to delay before creating the next installer pod.  We delay to avoid an LB outage (see godoc on minReadySeconds)
	sleepTime := c.timeToWaitBeforeInstallingNextPod(ctx, operatorStatus.NodeStatuses)
	if sleepTime > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(sleepTime):
		}
	}

	for l := 0; l < len(operatorStatus.NodeStatuses); l++ {
		i := (startNode + l) % len(operatorStatus.NodeStatuses)

		var currNodeState *operatorv1.NodeStatus
		var prevNodeState *operatorv1.NodeStatus
		currNodeState = &operatorStatus.NodeStatuses[i]
		if l > 0 {
			prev := (startNode + l - 1) % len(operatorStatus.NodeStatuses)
			prevNodeState = &operatorStatus.NodeStatuses[prev]
			nodeChoiceReason = fmt.Sprintf("node %s is the next node in the line", currNodeState.NodeName)
		}

		// if we are in a transition, check to see whether our installer pod completed
		if currNodeState.TargetRevision > currNodeState.CurrentRevision {
			if err := c.ensureInstallerPod(ctx, currNodeState.NodeName, operatorSpec, currNodeState.TargetRevision, currNodeState.LastFailedCount); err != nil {
				c.eventRecorder.Warningf("InstallerPodFailed", "Failed to create installer pod for revision %d count %d on node %q: %v",
					currNodeState.TargetRevision, currNodeState.NodeName, currNodeState.LastFailedCount, err)
				// if a newer revision is pending, continue, so we retry later with the latest available revision
				if !(operatorStatus.LatestAvailableRevision > currNodeState.TargetRevision) {
					return true, err
				}
			}

			newCurrNodeState, installerPodFailed, reason, err := c.newNodeStateForInstallInProgress(ctx, currNodeState, operatorStatus.LatestAvailableRevision)
			if err != nil {
				return true, err
			}

			// if we make a change to this status, we want to write it out to the API before we commence work on the next node.
			// it's an extra write/read, but it makes the state debuggable from outside this process
			if !equality.Semantic.DeepEqual(newCurrNodeState, currNodeState) {
				klog.Infof("%q moving to %v because %s", currNodeState.NodeName, spew.Sdump(*newCurrNodeState), reason)
				newOperatorStatus, updated, updateError := v1helpers.UpdateStaticPodStatus(c.operatorClient, setNodeStatusFn(newCurrNodeState), setAvailableProgressingNodeInstallerFailingConditions)
				if updateError != nil {
					return false, updateError
				} else if updated && currNodeState.CurrentRevision != newCurrNodeState.CurrentRevision {
					c.eventRecorder.Eventf("NodeCurrentRevisionChanged", "Updated node %q from revision %d to %d because %s", currNodeState.NodeName,
						currNodeState.CurrentRevision, newCurrNodeState.CurrentRevision, reason)
				}
				if err := c.updateRevisionStatus(ctx, newOperatorStatus); err != nil {
					klog.Errorf("error updating revision status configmap: %v", err)
				}
				return false, nil
			} else {
				klog.V(2).Infof("%q is in transition to %d, but has not made progress because %s", currNodeState.NodeName, currNodeState.TargetRevision, reasonWithBlame(reason))
			}

			// We want to retry the installer pod by deleting and then rekicking. Also we don't set LastFailedRevision.
			if !installerPodFailed {
				break
			}
			klog.Infof("Will retry %q for revision %d for the %s time because %s", currNodeState.NodeName, currNodeState.TargetRevision, nthTimeOr1st(newCurrNodeState.LastFailedCount), reason)
		}

		revisionToStart := c.getRevisionToStart(currNodeState, prevNodeState, operatorStatus)
		if revisionToStart == 0 {
			klog.V(4).Infof("%s, but node %s does not need update", nodeChoiceReason, currNodeState.NodeName)
			continue
		}

		if currNodeState.LastFailedRevision == revisionToStart && currNodeState.LastFailedTime != nil && !currNodeState.LastFailedTime.IsZero() {
			delay := backOffDuration(currNodeState.LastFailedCount)
			earliestRetry := currNodeState.LastFailedTime.Add(delay)
			if !c.now().After(earliestRetry) {
				klog.V(4).Infof("Backing off node %s installer retry %d until %v", currNodeState.NodeName, currNodeState.LastFailedCount+1, earliestRetry)
				return true, nil
			}
		}

		klog.Infof("%s and needs new revision %d", nodeChoiceReason, revisionToStart)

		newCurrNodeState := currNodeState.DeepCopy()
		newCurrNodeState.TargetRevision = revisionToStart
		if newCurrNodeState.LastFailedRevision != revisionToStart {
			newCurrNodeState.LastFailedRevisionErrors = nil
			newCurrNodeState.LastFailedCount = 0
			newCurrNodeState.LastFailedTime = nil
		} else if newCurrNodeState.LastFailedCount == 0 {
			newCurrNodeState.LastFailedCount = 1
		}

		// if we make a change to this status, we want to write it out to the API before we commence work on the next node.
		// it's an extra write/read, but it makes the state debuggable from outside this process
		if !equality.Semantic.DeepEqual(newCurrNodeState, currNodeState) {
			klog.Infof("%q moving to %v", currNodeState.NodeName, spew.Sdump(*newCurrNodeState))
			if _, updated, updateError := v1helpers.UpdateStaticPodStatus(c.operatorClient, setNodeStatusFn(newCurrNodeState), setAvailableProgressingNodeInstallerFailingConditions); updateError != nil {
				return false, updateError
			} else if updated && currNodeState.TargetRevision != newCurrNodeState.TargetRevision && newCurrNodeState.TargetRevision != 0 {
				c.eventRecorder.Eventf("NodeTargetRevisionChanged", "Updating node %q from revision %d to %d because %s", currNodeState.NodeName,
					currNodeState.CurrentRevision, newCurrNodeState.TargetRevision, nodeChoiceReason)
			}

			return false, nil
		}
		break
	}

	return false, nil
}

func (c *InstallerController) updateRevisionStatus(ctx context.Context, operatorStatus *operatorv1.StaticPodOperatorStatus) error {
	failedRevisions := make(map[int32]struct{})
	currentRevisions := make(map[int32]struct{})
	for _, nodeState := range operatorStatus.NodeStatuses {
		failedRevisions[nodeState.LastFailedRevision] = struct{}{}
		currentRevisions[nodeState.CurrentRevision] = struct{}{}
	}
	delete(failedRevisions, 0)

	// If all current revisions point to the same revision, then mark it successful
	if len(currentRevisions) == 1 {
		err := c.updateConfigMapForRevision(ctx, currentRevisions, string(corev1.PodSucceeded))
		if err != nil {
			return err
		}
	}
	return c.updateConfigMapForRevision(ctx, failedRevisions, string(corev1.PodFailed))
}

func (c *InstallerController) updateConfigMapForRevision(ctx context.Context, currentRevisions map[int32]struct{}, status string) error {
	for currentRevision := range currentRevisions {
		statusConfigMap, err := c.configMapsGetter.ConfigMaps(c.targetNamespace).Get(ctx, statusConfigMapNameForRevision(currentRevision), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			klog.Infof("%s configmap not found, skipping update revision status", statusConfigMapNameForRevision(currentRevision))
			continue
		}
		if err != nil {
			return err
		}
		statusConfigMap.Data["status"] = status
		_, _, err = resourceapply.ApplyConfigMap(ctx, c.configMapsGetter, c.eventRecorder, statusConfigMap)
		if err != nil {
			return err
		}
	}
	return nil
}

func setNodeStatusFn(status *operatorv1.NodeStatus) v1helpers.UpdateStaticPodStatusFunc {
	return func(operatorStatus *operatorv1.StaticPodOperatorStatus) error {
		for i := range operatorStatus.NodeStatuses {
			if operatorStatus.NodeStatuses[i].NodeName == status.NodeName {
				operatorStatus.NodeStatuses[i] = *status
				break
			}
		}
		return nil
	}
}

// setAvailableProgressingConditions sets the Available and Progressing conditions
func setAvailableProgressingNodeInstallerFailingConditions(newStatus *operatorv1.StaticPodOperatorStatus) error {
	// Available means that we have at least one pod at the latest level
	numAvailable := 0
	numAtLatestRevision := 0
	numProgressing := 0
	counts := map[int32]int{}
	failingCount := map[int32]int{}
	failing := map[int32][]string{}
	for _, currNodeStatus := range newStatus.NodeStatuses {
		counts[currNodeStatus.CurrentRevision] = counts[currNodeStatus.CurrentRevision] + 1
		if currNodeStatus.CurrentRevision != 0 {
			numAvailable++
		}

		// keep track of failures so that we can report failing status
		if currNodeStatus.LastFailedRevision != 0 {
			failingCount[currNodeStatus.LastFailedRevision] = failingCount[currNodeStatus.LastFailedRevision] + 1
			failing[currNodeStatus.LastFailedRevision] = append(failing[currNodeStatus.LastFailedRevision], currNodeStatus.LastFailedRevisionErrors...)
		}

		if newStatus.LatestAvailableRevision == currNodeStatus.CurrentRevision {
			numAtLatestRevision += 1
		} else {
			numProgressing += 1
		}
	}

	revisionStrings := []string{}
	for _, currentRevision := range Int32KeySet(counts).List() {
		count := counts[currentRevision]
		revisionStrings = append(revisionStrings, fmt.Sprintf("%d nodes are at revision %d", count, currentRevision))
	}
	// if we are progressing and no nodes have achieved that level, we should indicate
	if numProgressing > 0 && counts[newStatus.LatestAvailableRevision] == 0 {
		revisionStrings = append(revisionStrings, fmt.Sprintf("%d nodes have achieved new revision %d", 0, newStatus.LatestAvailableRevision))
	}
	revisionDescription := strings.Join(revisionStrings, "; ")

	if numAvailable > 0 {
		v1helpers.SetOperatorCondition(&newStatus.Conditions, operatorv1.OperatorCondition{
			Type:    condition.StaticPodsAvailableConditionType,
			Status:  operatorv1.ConditionTrue,
			Message: fmt.Sprintf("%d nodes are active; %s", numAvailable, revisionDescription),
		})
	} else {
		v1helpers.SetOperatorCondition(&newStatus.Conditions, operatorv1.OperatorCondition{
			Type:    condition.StaticPodsAvailableConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "ZeroNodesActive",
			Message: fmt.Sprintf("%d nodes are active; %s", numAvailable, revisionDescription),
		})
	}

	// Progressing means that the any node is not at the latest available revision
	if numProgressing > 0 {
		v1helpers.SetOperatorCondition(&newStatus.Conditions, operatorv1.OperatorCondition{
			Type:    condition.NodeInstallerProgressingConditionType,
			Status:  operatorv1.ConditionTrue,
			Message: fmt.Sprintf("%s", revisionDescription),
		})
	} else {
		v1helpers.SetOperatorCondition(&newStatus.Conditions, operatorv1.OperatorCondition{
			Type:    condition.NodeInstallerProgressingConditionType,
			Status:  operatorv1.ConditionFalse,
			Reason:  "AllNodesAtLatestRevision",
			Message: fmt.Sprintf("%s", revisionDescription),
		})
	}

	if len(failing) > 0 {
		failingStrings := []string{}
		for _, failingRevision := range Int32KeySet(failing).List() {
			errorStrings := failing[failingRevision]
			failingStrings = append(failingStrings, fmt.Sprintf("%d nodes are failing on revision %d:\n%v", failingCount[failingRevision], failingRevision, strings.Join(errorStrings, "\n")))
		}
		failingDescription := strings.Join(failingStrings, "; ")

		v1helpers.SetOperatorCondition(&newStatus.Conditions, operatorv1.OperatorCondition{
			Type:    condition.NodeInstallerDegradedConditionType,
			Status:  operatorv1.ConditionTrue,
			Reason:  "InstallerPodFailed",
			Message: failingDescription,
		})
	} else {
		v1helpers.SetOperatorCondition(&newStatus.Conditions, operatorv1.OperatorCondition{
			Type:   condition.NodeInstallerDegradedConditionType,
			Status: operatorv1.ConditionFalse,
		})
	}

	return nil
}

// newNodeStateForInstallInProgress returns the new NodeState
func (c *InstallerController) newNodeStateForInstallInProgress(ctx context.Context, currNodeState *operatorv1.NodeStatus, latestRevisionAvailable int32) (status *operatorv1.NodeStatus, installerPodFailed bool, reason string, err error) {
	ret := currNodeState.DeepCopy()

	// how many previously failed of latest available revision
	previouslyFailedLatestRevisionPods := 0
	if currNodeState.LastFailedRevision != 0 && currNodeState.LastFailedRevision == currNodeState.TargetRevision {
		previouslyFailedLatestRevisionPods = max(1, currNodeState.LastFailedCount)
	}

	installerPodName := getInstallerPodName(currNodeState.TargetRevision, currNodeState.NodeName, previouslyFailedLatestRevisionPods)
	installerPod, err := c.podsGetter.Pods(c.targetNamespace).Get(ctx, installerPodName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// installer pod has disappeared before we saw it's termination state. Retry like if it had never existed.
		c.eventRecorder.Warning("InstallerPodDisappeared", err.Error())
		return currNodeState, true, fmt.Sprintf("pod %s disappeared", installerPodName), nil
	}
	if err != nil {
		return nil, false, "", err
	}

	// quickly move to new revision even when an old installer has been seen, if necessary with force by
	// deleting the old installer (it might be frozen).
	if pendingNewRevision := latestRevisionAvailable > currNodeState.TargetRevision; pendingNewRevision {
		switch installerPod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			// stop early, don't wait for ready static operand pod because a new revision is waiting
		default:
			// delete non-terminated pod. It may be in some state where it would never terminate, e.g. ContainerCreating
			if err := c.podsGetter.Pods(c.targetNamespace).Delete(ctx, installerPodName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return ret, false, "", err
			}
		}

		ret.LastFailedRevision = 0
		ret.LastFailedTime = nil
		ret.LastFailedCount = 0
		ret.TargetRevision = 0
		ret.LastFailedRevisionErrors = nil

		return ret, false, "new revision pending", nil
	}

	errors := []string{}
	reason = ""

	switch installerPod.Status.Phase {
	case corev1.PodSucceeded:
		state, currentRevision, staticPodReason, failedErrors, err := c.getStaticPodState(ctx, currNodeState.NodeName)
		if err != nil && apierrors.IsNotFound(err) {
			// pod not launched yet
			// TODO: have a timeout here and retry the installer
			reason = "static pod is pending"
			break
		}
		if err != nil {
			return nil, false, "", err
		}

		if currentRevision != strconv.Itoa(int(currNodeState.TargetRevision)) {
			// new updated pod to be launched
			if len(currentRevision) == 0 {
				reason = fmt.Sprintf("waiting for static pod of revision %d", currNodeState.TargetRevision)
			} else {
				reason = fmt.Sprintf("waiting for static pod of revision %d, found %s", currNodeState.TargetRevision, currentRevision)
			}
			break
		}

		switch state {
		case staticPodStateFailed:
			reason = staticPodReason
			errors = failedErrors

			ret.TargetRevision = 0 // stop installer retries
			ret.LastFailedRevision = currNodeState.TargetRevision
			now := metav1.NewTime(c.now())
			ret.LastFailedTime = &now
			ret.LastFailedCount++
			ns, name := c.targetNamespace, mirrorPodNameForNode(c.staticPodName, currNodeState.NodeName)
			if len(errors) == 0 {
				errors = append(errors, fmt.Sprintf("no detailed termination message, see `oc get -oyaml -n %q pods %q`", ns, name))
			}
			ret.LastFailedRevisionErrors = errors

			return ret, false, fmt.Sprintf("operand pod failed: %v", strings.Join(errors, "\n")), nil

		case staticPodStateReady:
			if currNodeState.TargetRevision > ret.CurrentRevision {
				ret.CurrentRevision = currNodeState.TargetRevision
			}
			ret.TargetRevision = 0
			ret.LastFailedRevision = 0
			ret.LastFailedTime = nil
			ret.LastFailedCount = 0
			ret.LastFailedRevisionErrors = nil
			return ret, false, staticPodReason, nil
		default:
			reason = "static pod is pending"
		}

	case corev1.PodFailed:
		reason = "installer pod failed"
		for _, containerStatus := range installerPod.Status.ContainerStatuses {
			if containerStatus.State.Terminated != nil && len(containerStatus.State.Terminated.Message) > 0 {
				errors = append(errors, fmt.Sprintf("%s: %s", containerStatus.Name, containerStatus.State.Terminated.Message))
				c.eventRecorder.Warningf("InstallerPodFailed", "installer errors: %v", strings.Join(errors, "\n"))
			}
		}

		ret.LastFailedRevision = currNodeState.TargetRevision
		now := metav1.NewTime(c.now())
		ret.LastFailedTime = &now
		ret.LastFailedCount++
		if len(errors) == 0 {
			errors = append(errors, fmt.Sprintf("no detailed termination message, see `oc get -oyaml -n %q pods %q`", installerPod.Namespace, installerPod.Name))
		}
		ret.LastFailedRevisionErrors = errors
		return ret, true, fmt.Sprintf("installer pod failed: %v", strings.Join(errors, "\n")), nil

	default:
		if len(installerPod.Status.Message) > 0 {
			reason = fmt.Sprintf("installer is not finished: %s", installerPod.Status.Message)
		} else {
			reason = fmt.Sprintf("installer is not finished, but in %s phase", installerPod.Status.Phase)
		}
	}

	return ret, false, reason, nil
}

// getRevisionToStart returns the revision we need to start or zero if none
func (c *InstallerController) getRevisionToStart(currNodeState, prevNodeState *operatorv1.NodeStatus, operatorStatus *operatorv1.StaticPodOperatorStatus) int32 {
	if prevNodeState == nil {
		currentAtLatest := currNodeState.CurrentRevision == operatorStatus.LatestAvailableRevision
		if !currentAtLatest {
			return operatorStatus.LatestAvailableRevision
		}
		return 0
	}

	prevFinished := prevNodeState.TargetRevision == 0
	prevInTransition := prevNodeState.CurrentRevision != prevNodeState.TargetRevision
	if prevInTransition && !prevFinished {
		return 0
	}

	prevAhead := prevNodeState.CurrentRevision > currNodeState.CurrentRevision
	failedAtPrev := currNodeState.LastFailedRevision == prevNodeState.CurrentRevision
	if prevAhead && !failedAtPrev {
		return prevNodeState.CurrentRevision
	}

	return 0
}

func getInstallerPodName(revision int32, nodeName string, previouslyFailedRevisionPods int) string {
	if previouslyFailedRevisionPods == 0 {
		return fmt.Sprintf("installer-%d-%s", revision, nodeName)
	}
	return fmt.Sprintf("installer-%d-retry-%d-%s", revision, previouslyFailedRevisionPods, nodeName)
}

// ensureInstallerPod creates the installer pod with the secrets required to if it does not exist already
func (c *InstallerController) ensureInstallerPod(ctx context.Context, nodeName string, operatorSpec *operatorv1.StaticPodOperatorSpec, revision int32, previouslyFailedRevisionPods int) error {
	pod := resourceread.ReadPodV1OrDie(bindata.MustAsset(filepath.Join(manifestDir, manifestInstallerPodPath)))

	pod.Namespace = c.targetNamespace
	pod.Name = getInstallerPodName(revision, nodeName, previouslyFailedRevisionPods)
	pod.Spec.NodeName = nodeName
	pod.Spec.Containers[0].Image = c.installerPodImageFn()
	pod.Spec.Containers[0].Command = c.command

	ownerRefs, err := c.ownerRefsFn(revision)
	if err != nil {
		return fmt.Errorf("unable to set installer pod ownerrefs: %+v", err)
	}
	pod.OwnerReferences = ownerRefs

	if c.configMaps[0].Optional {
		return fmt.Errorf("pod configmap %s is required, cannot be optional", c.configMaps[0].Name)
	}

	args := []string{
		fmt.Sprintf("-v=%d", loglevel.LogLevelToVerbosity(operatorSpec.LogLevel)),
		fmt.Sprintf("--revision=%d", revision),
		fmt.Sprintf("--namespace=%s", pod.Namespace),
		fmt.Sprintf("--pod=%s", c.configMaps[0].Name),
		fmt.Sprintf("--resource-dir=%s", hostResourceDirDir),
		fmt.Sprintf("--pod-manifest-dir=%s", hostPodManifestDir),
	}
	for _, cm := range c.configMaps {
		if cm.Optional {
			args = append(args, fmt.Sprintf("--optional-configmaps=%s", cm.Name))
		} else {
			args = append(args, fmt.Sprintf("--configmaps=%s", cm.Name))
		}
	}
	for _, s := range c.secrets {
		if s.Optional {
			args = append(args, fmt.Sprintf("--optional-secrets=%s", s.Name))
		} else {
			args = append(args, fmt.Sprintf("--secrets=%s", s.Name))
		}
	}
	if len(c.certDir) > 0 {
		args = append(args, fmt.Sprintf("--cert-dir=%s", filepath.Join(hostResourceDirDir, c.certDir)))
		for _, cm := range c.certConfigMaps {
			if cm.Optional {
				args = append(args, fmt.Sprintf("--optional-cert-configmaps=%s", cm.Name))
			} else {
				args = append(args, fmt.Sprintf("--cert-configmaps=%s", cm.Name))
			}
		}
		for _, s := range c.certSecrets {
			if s.Optional {
				args = append(args, fmt.Sprintf("--optional-cert-secrets=%s", s.Name))
			} else {
				args = append(args, fmt.Sprintf("--cert-secrets=%s", s.Name))
			}
		}
	}

	pod.Spec.Containers[0].Args = args

	// Some owners need to change aspects of the pod.  Things like arguments for instance
	for _, fn := range c.installerPodMutationFns {
		if err := fn(pod, nodeName, operatorSpec, revision); err != nil {
			return err
		}
	}

	_, _, err = resourceapply.ApplyPod(ctx, c.podsGetter, c.eventRecorder, pod)
	return err
}

func (c *InstallerController) setOwnerRefs(revision int32) ([]metav1.OwnerReference, error) {
	ownerReferences := []metav1.OwnerReference{}
	statusConfigMap, err := c.configMapsGetter.ConfigMaps(c.targetNamespace).Get(context.TODO(), fmt.Sprintf("revision-status-%d", revision), metav1.GetOptions{})
	if err == nil {
		ownerReferences = append(ownerReferences, metav1.OwnerReference{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       statusConfigMap.Name,
			UID:        statusConfigMap.UID,
		})
	}
	return ownerReferences, err
}

func getInstallerPodImageFromEnv() string {
	return os.Getenv("OPERATOR_IMAGE")
}

func (c InstallerController) ensureSecretRevisionResourcesExists(ctx context.Context, secrets []revision.RevisionResource, latestRevisionNumber int32) error {
	missing := sets.NewString()
	for _, secret := range secrets {
		if secret.Optional {
			continue
		}
		name := fmt.Sprintf("%s-%d", secret.Name, latestRevisionNumber)
		_, err := c.secretsGetter.Secrets(c.targetNamespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			continue
		}
		if apierrors.IsNotFound(err) {
			missing.Insert(name)
		}
	}
	if missing.Len() == 0 {
		return nil
	}
	return fmt.Errorf("secrets: %s", strings.Join(missing.List(), ","))
}

func (c InstallerController) ensureConfigMapRevisionResourcesExists(ctx context.Context, configs []revision.RevisionResource, latestRevisionNumber int32) error {
	missing := sets.NewString()
	for _, config := range configs {
		if config.Optional {
			continue
		}
		name := fmt.Sprintf("%s-%d", config.Name, latestRevisionNumber)
		_, err := c.configMapsGetter.ConfigMaps(c.targetNamespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			continue
		}
		if apierrors.IsNotFound(err) {
			missing.Insert(name)
		}
	}
	if missing.Len() == 0 {
		return nil
	}
	return fmt.Errorf("configmaps: %s", strings.Join(missing.List(), ","))
}

func (c InstallerController) ensureUnrevisionedSecretResourcesExists(ctx context.Context, secrets []UnrevisionedResource) error {
	missing := sets.NewString()
	for _, secret := range secrets {
		if secret.Optional {
			continue
		}
		_, err := c.secretsGetter.Secrets(c.targetNamespace).Get(ctx, secret.Name, metav1.GetOptions{})
		if err == nil {
			continue
		}
		if apierrors.IsNotFound(err) {
			missing.Insert(secret.Name)
		}
	}
	if missing.Len() == 0 {
		return nil
	}
	return fmt.Errorf("secrets: %s", strings.Join(missing.List(), ","))
}

func (c InstallerController) ensureUnrevisionedConfigMapResourcesExists(ctx context.Context, configs []UnrevisionedResource) error {
	missing := sets.NewString()
	for _, config := range configs {
		if config.Optional {
			continue
		}
		_, err := c.configMapsGetter.ConfigMaps(c.targetNamespace).Get(ctx, config.Name, metav1.GetOptions{})
		if err == nil {
			continue
		}
		if apierrors.IsNotFound(err) {
			missing.Insert(config.Name)
		}
	}
	if missing.Len() == 0 {
		return nil
	}
	return fmt.Errorf("configmaps: %s", strings.Join(missing.List(), ","))
}

// ensureRequiredResourcesExist makes sure that all non-optional resources are ready or it will return an error to trigger a requeue so that we try again.
func (c InstallerController) ensureRequiredResourcesExist(ctx context.Context, revisionNumber int32) error {
	errs := []error{}

	errs = append(errs, c.ensureUnrevisionedConfigMapResourcesExists(ctx, c.certConfigMaps))
	errs = append(errs, c.ensureUnrevisionedSecretResourcesExists(ctx, c.certSecrets))

	errs = append(errs, c.ensureConfigMapRevisionResourcesExists(ctx, c.configMaps, revisionNumber))
	errs = append(errs, c.ensureSecretRevisionResourcesExists(ctx, c.secrets, revisionNumber))

	aggregatedErr := utilerrors.NewAggregate(errs)
	if aggregatedErr == nil {
		return nil
	}

	eventMessages := []string{}
	for _, err := range aggregatedErr.Errors() {
		eventMessages = append(eventMessages, err.Error())
	}
	c.eventRecorder.Warningf("RequiredInstallerResourcesMissing", strings.Join(eventMessages, ", "))
	return fmt.Errorf("missing required resources: %v", aggregatedErr)
}

func (c InstallerController) Sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, originalOperatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}
	operatorStatus := originalOperatorStatus.DeepCopy()

	if !management.IsOperatorManaged(operatorSpec.ManagementState) {
		return nil
	}

	err = c.ensureRequiredResourcesExist(ctx, originalOperatorStatus.LatestAvailableRevision)

	// Only manage installation pods when all required certs are present.
	if err == nil {
		requeue, syncErr := c.manageInstallationPods(ctx, operatorSpec, operatorStatus)
		if requeue && syncErr == nil {
			return factory.SyntheticRequeueError
		}
		err = syncErr
	}

	// Update failing condition
	// If required certs are missing, this will report degraded as we can't create installer pods because of this pre-condition.
	cond := operatorv1.OperatorCondition{
		Type:   condition.InstallerControllerDegradedConditionType,
		Status: operatorv1.ConditionFalse,
	}
	if err != nil {
		cond.Status = operatorv1.ConditionTrue
		cond.Reason = "Error"
		cond.Message = err.Error()
	}
	if _, _, updateError := v1helpers.UpdateStaticPodStatus(c.operatorClient, v1helpers.UpdateStaticPodConditionFn(cond), setAvailableProgressingNodeInstallerFailingConditions); updateError != nil {
		if err == nil {
			return updateError
		}
	}

	return err
}

func mirrorPodNameForNode(staticPodName, nodeName string) string {
	return staticPodName + "-" + nodeName
}

func statusConfigMapNameForRevision(revision int32) string {
	return fmt.Sprintf("%s-%d", statusConfigMapName, revision)
}

func backOffDuration(count int) time.Duration {
	d := time.Second * time.Duration(float64(10)*math.Pow(1.5, float64(count)))
	if d > time.Minute*10 {
		return time.Minute * 10
	}
	return d
}

func nthTimeOr1st(n int) string {
	switch {
	case n == 0:
		return "1st"
	case n%10 == 1 && n%100 != 11:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2 && n%100 != 12:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3 && n%100 != 13:
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}

func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

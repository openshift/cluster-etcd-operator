package prune

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const maxInt32 = 2147483647

// PruneController is a controller that watches static installer pod revision statuses and spawns
// a pruner pod to delete old revision resources from disk
type PruneController struct {
	targetNamespace, podResourcePrefix, certDir string
	// command is the string to use for the pruning pod command
	command []string

	// prunerPodImageFn returns the image name for the pruning pod
	prunerPodImageFn func() string
	// retrieveStatusConfigMapOwnerRefsFn gets the revision status ConfigMap and returns an owner ref, or empty slice on error.
	retrieveStatusConfigMapOwnerRefsFn func(ctx context.Context, revision int32) ([]metav1.OwnerReference, error)

	operatorClient v1helpers.StaticPodOperatorClient

	configMapGetter corev1client.ConfigMapsGetter
	podGetter       corev1client.PodsGetter
}

const (
	statusConfigMapName  = "revision-status-"
	defaultRevisionLimit = int32(5)
)

// NewPruneController creates a new pruning controller
func NewPruneController(
	targetNamespace string,
	podResourcePrefix string,
	command []string,
	configMapGetter corev1client.ConfigMapsGetter,
	podGetter corev1client.PodsGetter,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &PruneController{
		targetNamespace:   targetNamespace,
		podResourcePrefix: podResourcePrefix,
		command:           command,

		operatorClient:  operatorClient,
		configMapGetter: configMapGetter,
		podGetter:       podGetter,

		prunerPodImageFn: getPrunerPodImageFromEnv,
	}
	c.retrieveStatusConfigMapOwnerRefsFn = c.createStatusConfigMapOwnerRefs

	return factory.New().
		WithInformers(
			operatorClient.Informer(),
			kubeInformersForTargetNamespace.Core().V1().ConfigMaps().Informer(),
		).
		WithSync(c.sync).
		ToController(
			"PruneController", // don't change what is passed here unless you also remove the old FooDegraded condition
			eventRecorder,
		)
}

func defaultedLimits(operatorSpec *operatorv1.StaticPodOperatorSpec) (int, int) {
	failedRevisionLimit := defaultRevisionLimit
	succeededRevisionLimit := defaultRevisionLimit
	if operatorSpec.FailedRevisionLimit != 0 {
		failedRevisionLimit = operatorSpec.FailedRevisionLimit
	}
	if operatorSpec.SucceededRevisionLimit != 0 {
		succeededRevisionLimit = operatorSpec.SucceededRevisionLimit
	}
	return int(failedRevisionLimit), int(succeededRevisionLimit)
}

// revisionsToKeep approximates the set of revisions to keep: spec.failedRevisionsLimit for failed revisions,
// spec.succeededRevisionsLimit for succeed revisions (for all nodes). The approximation goes by:
// - don't prune LatestAvailableRevision and the max(spec.failedRevisionLimit, spec.succeededRevisionLimit) - 1 revisions before it.
// - don't prune a node's CurrentRevision and the spec.succeededRevisionLimit - 1 revisions before it.
// - don't prune a node's TargetRevision and the spec.failedRevisionLimit - 1 revisions before it.
// - don't prune a node's LastFailedRevision and the spec.failedRevisionLimit - 1 revisions before it.
func (c *PruneController) revisionsToKeep(status *operatorv1.StaticPodOperatorStatus, failedLimit, succeededLimit int) (all bool, keep sets.Set[int32]) {
	// find oldest where we are sure it cannot fail anymore (i.e. = currentRevision
	var oldestSucceeded int32 = maxInt32
	for _, ns := range status.NodeStatuses {
		if ns.CurrentRevision < oldestSucceeded {
			oldestSucceeded = ns.CurrentRevision
		}
	}
	if oldestSucceeded < status.LatestAvailableRevision && failedLimit == -1 {
		return true, nil
	}
	if succeededLimit == -1 {
		return true, nil
	}

	keep = sets.Set[int32]{}
	if oldestSucceeded < status.LatestAvailableRevision {
		keep.Insert(int32RangeBelowOrEqual(status.LatestAvailableRevision, maxLimit(failedLimit, succeededLimit))...) // max because we don't know about failure or success
	} // otherwise all nodes are on LatestAvailableRevision already. Then there is no fail potential.

	for _, ns := range status.NodeStatuses {
		if ns.CurrentRevision > 0 {
			keep.Insert(int32RangeBelowOrEqual(ns.CurrentRevision, succeededLimit)...)
		}
		if ns.TargetRevision > 0 {
			keep.Insert(int32RangeBelowOrEqual(ns.TargetRevision, maxLimit(failedLimit, succeededLimit))...) // max because we don't know about failure or success
		}
		if ns.LastFailedRevision > 0 {
			keep.Insert(int32RangeBelowOrEqual(ns.LastFailedRevision, failedLimit)...)
		}
	}

	if keep.Len() > 0 && sets.List(keep)[0] == 1 && sets.List(keep)[keep.Len()-1] == status.LatestAvailableRevision {
		return true, nil
	}

	return false, keep
}

// int32Range returns range of int32 from upper-num+1 to upper.
func int32RangeBelowOrEqual(upper int32, num int) []int32 {
	ret := make([]int32, 0, num)
	for i := 0; i < num; i++ {
		value := upper - int32(num) + 1 + int32(i)
		if value > 0 {
			ret = append(ret, value)
		}
	}
	return ret
}

func (c *PruneController) pruneDiskResources(ctx context.Context, recorder events.Recorder, operatorStatus *operatorv1.StaticPodOperatorStatus, toKeep []int32) error {
	// Run pruning pod on each node and pin it to that node
	for _, nodeStatus := range operatorStatus.NodeStatuses {
		// note: we attach the pod (via owner-ref) to the latestAvailable
		if err := c.ensurePrunePod(ctx, recorder, nodeStatus.NodeName, operatorStatus.LatestAvailableRevision, toKeep, operatorStatus.LatestAvailableRevision); err != nil {
			return err
		}
	}
	return nil
}

func (c *PruneController) pruneAPIResources(ctx context.Context, toKeep sets.Set[int32], latestAvailableRevision int32) error {
	statusConfigMaps, err := c.configMapGetter.ConfigMaps(c.targetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, cm := range statusConfigMaps.Items {
		if !strings.HasPrefix(cm.Name, statusConfigMapName) {
			continue
		}

		revision, err := strconv.Atoi(cm.Data["revision"])
		if err != nil {
			return fmt.Errorf("unexpected error converting revision to int: %+v", err)
		}

		if toKeep.Has(int32(revision)) {
			continue
		}
		if revision > int(latestAvailableRevision) {
			continue
		}
		if err := c.configMapGetter.ConfigMaps(c.targetNamespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
	}
	return nil
}

//go:embed manifests/pruner-pod.yaml
var podTemplate []byte

func (c *PruneController) ensurePrunePod(ctx context.Context, recorder events.Recorder, nodeName string, maxEligibleRevision int32, protectedRevisions []int32, revision int32) error {
	if revision == 0 {
		return nil
	}
	pod := resourceread.ReadPodV1OrDie(podTemplate)

	pod.Name = getPrunerPodName(nodeName, revision)
	pod.Namespace = c.targetNamespace
	pod.Spec.NodeName = nodeName
	pod.Spec.Containers[0].Image = c.prunerPodImageFn()
	pod.Spec.Containers[0].Command = c.command
	pod.Spec.Containers[0].Args = append(pod.Spec.Containers[0].Args,
		fmt.Sprintf("-v=%d", 4),
		fmt.Sprintf("--max-eligible-revision=%d", maxEligibleRevision),
		fmt.Sprintf("--protected-revisions=%s", revisionsToString(protectedRevisions)),
		fmt.Sprintf("--resource-dir=%s", "/etc/kubernetes/static-pod-resources"),
		fmt.Sprintf("--static-pod-name=%s", c.podResourcePrefix),
	)

	ownerRefs, err := c.retrieveStatusConfigMapOwnerRefsFn(ctx, revision)
	if err != nil {
		return fmt.Errorf("unable to set pruner pod ownerrefs: %+v", err)
	}
	pod.OwnerReferences = ownerRefs

	_, _, err = resourceapply.ApplyPod(ctx, c.podGetter, recorder, pod)
	return err
}

func (c *PruneController) createStatusConfigMapOwnerRefs(ctx context.Context, revision int32) ([]metav1.OwnerReference, error) {
	statusConfigMap, err := c.configMapGetter.ConfigMaps(c.targetNamespace).Get(ctx, fmt.Sprintf("revision-status-%d", revision), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return []metav1.OwnerReference{
		{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       statusConfigMap.Name,
			UID:        statusConfigMap.UID,
		},
	}, nil
}

func getPrunerPodName(nodeName string, revision int32) string {
	return fmt.Sprintf("revision-pruner-%d-%s", revision, nodeName)
}

func revisionsToString(revisions []int32) string {
	values := []string{}
	for _, id := range revisions {
		value := strconv.Itoa(int(id))
		values = append(values, value)
	}
	return strings.Join(values, ",")
}

func getPrunerPodImageFromEnv() string {
	return os.Getenv("OPERATOR_IMAGE")
}

func (c *PruneController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(5).Info("Syncing revision pruner")
	operatorSpec, operatorStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	if len(operatorStatus.NodeStatuses) == 0 {
		klog.Info("No nodes, nothing to prune")
		return nil
	}

	// keep a number of revision before current, target, last failed and last available revisions
	failedLimit, succeededLimit := defaultedLimits(operatorSpec)
	keepAll, toKeep := c.revisionsToKeep(operatorStatus, failedLimit, succeededLimit)
	if keepAll {
		klog.Info("Nothing to prune")
		return nil
	}

	errs := []error{}
	if diskErr := c.pruneDiskResources(ctx, syncCtx.Recorder(), operatorStatus, sets.List(toKeep)); diskErr != nil {
		errs = append(errs, diskErr)
	}
	if apiErr := c.pruneAPIResources(ctx, toKeep, operatorStatus.LatestAvailableRevision); apiErr != nil {
		errs = append(errs, apiErr)
	}
	return v1helpers.NewMultiLineAggregate(errs)
}

func maxLimit(a, b int) int {
	if a < 0 || b < 0 {
		return -1
	}
	if a > b {
		return a
	}
	return b
}

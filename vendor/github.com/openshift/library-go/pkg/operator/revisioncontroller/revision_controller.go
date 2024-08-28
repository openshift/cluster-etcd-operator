package revisioncontroller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// LatestRevisionClient is an operator client for an operator status with a latest revision field.
type LatestRevisionClient interface {
	v1helpers.OperatorClient

	// GetLatestRevisionState returns the spec, status and latest revision.
	GetLatestRevisionState() (spec *operatorv1.OperatorSpec, status *operatorv1.OperatorStatus, rev int32, rv string, err error)
	// UpdateLatestRevisionOperatorStatus updates the status with the given latestAvailableRevision and the by applying the given updateFuncs.
	UpdateLatestRevisionOperatorStatus(ctx context.Context, latestAvailableRevision int32, updateFuncs ...v1helpers.UpdateStatusFunc) (*operatorv1.OperatorStatus, bool, error)
}

// RevisionController is a controller that watches a set of configmaps and secrets and them against a revision snapshot
// of them. If the original resources changes, the revision counter is increased, stored in LatestAvailableRevision
// field of the operator config and new snapshots suffixed by the revision are created.
type RevisionController struct {
	targetNamespace string
	// configMaps is the list of configmaps that are directly copied.A different actor/controller modifies these.
	// the first element should be the configmap that contains the static pod manifest
	configMaps []RevisionResource
	// secrets is a list of secrets that are directly copied for the current values.  A different actor/controller modifies these.
	secrets []RevisionResource

	operatorClient  LatestRevisionClient
	configMapGetter corev1client.ConfigMapsGetter
	secretGetter    corev1client.SecretsGetter

	revisionPrecondition PreconditionFunc
}

type RevisionResource struct {
	Name     string
	Optional bool
}

// PreconditionFunc checks if revision precondition is met (is true) and then proceeeds with the creation of new revision
type PreconditionFunc func(ctx context.Context) (bool, error)

// NewRevisionController create a new revision controller.
func NewRevisionController(
	targetNamespace string,
	configMaps []RevisionResource,
	secrets []RevisionResource,
	kubeInformersForTargetNamespace informers.SharedInformerFactory,
	operatorClient LatestRevisionClient,
	configMapGetter corev1client.ConfigMapsGetter,
	secretGetter corev1client.SecretsGetter,
	eventRecorder events.Recorder,
	revisionPrecondition PreconditionFunc,
) factory.Controller {
	if revisionPrecondition == nil {
		revisionPrecondition = func(ctx context.Context) (bool, error) {
			return true, nil
		}
	}

	c := &RevisionController{
		targetNamespace: targetNamespace,
		configMaps:      configMaps,
		secrets:         secrets,

		operatorClient:       operatorClient,
		configMapGetter:      configMapGetter,
		secretGetter:         secretGetter,
		revisionPrecondition: revisionPrecondition,
	}

	return factory.New().WithInformers(
		operatorClient.Informer(),
		kubeInformersForTargetNamespace.Core().V1().ConfigMaps().Informer(),
		kubeInformersForTargetNamespace.Core().V1().Secrets().Informer()).WithSync(c.sync).ResyncEvery(1*time.Minute).ToController("RevisionController", eventRecorder)
}

// createRevisionIfNeeded takes care of creating content for the static pods to use.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func (c RevisionController) createRevisionIfNeeded(ctx context.Context, recorder events.Recorder, latestAvailableRevision int32, resourceVersion string) (bool, error) {
	isLatestRevisionCurrent, requiredIsNotFound, reason := c.isLatestRevisionCurrent(ctx, latestAvailableRevision)

	// check to make sure that the latestRevision has the exact content we expect.  No mutation here, so we start creating the next Revision only when it is required
	if isLatestRevisionCurrent {
		klog.V(4).Infof("Returning early, %d triggered and up to date", latestAvailableRevision)
		return false, nil
	}

	nextRevision := latestAvailableRevision + 1
	var createdNewRevision bool
	var err error
	// check to make sure no new revision is created when a required object is missing
	if requiredIsNotFound {
		err = fmt.Errorf("%v", reason)
	} else {
		recorder.Eventf("StartingNewRevision", "new revision %d triggered by %q", nextRevision, reason)
		createdNewRevision, err = c.createNewRevision(ctx, recorder, nextRevision, reason)
	}

	if err != nil {
		cond := operatorv1.OperatorCondition{
			Type:    "RevisionControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ContentCreationError",
			Message: err.Error(),
		}
		if _, _, updateError := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(cond)); updateError != nil {
			recorder.Warningf("RevisionCreateFailed", "Failed to create revision %d: %v", nextRevision, err.Error())
			return true, updateError
		}
		return true, nil
	}

	if !createdNewRevision {
		klog.V(4).Infof("Revision %v not created", nextRevision)
		return false, nil
	}
	recorder.Eventf("RevisionTriggered", "new revision %d triggered by %q", nextRevision, reason)

	cond := operatorv1.OperatorCondition{
		Type:   "RevisionControllerDegraded",
		Status: operatorv1.ConditionFalse,
	}
	if _, updated, updateError := c.operatorClient.UpdateLatestRevisionOperatorStatus(ctx, nextRevision, v1helpers.UpdateConditionFn(cond)); updateError != nil {
		return true, updateError
	} else if updated {
		recorder.Eventf("RevisionCreate", "Revision %d created because %s", nextRevision, reason)
	}

	return false, nil
}

func nameFor(name string, revision int32) string {
	return fmt.Sprintf("%s-%d", name, revision)
}

// isLatestRevisionCurrent returns whether the latest revision is up to date and an optional reason
func (c RevisionController) isLatestRevisionCurrent(ctx context.Context, revision int32) (bool, bool, string) {
	configChanges := []string{}
	for _, cm := range c.configMaps {
		requiredData := map[string]string{}
		existingData := map[string]string{}

		required, err := c.configMapGetter.ConfigMaps(c.targetNamespace).Get(ctx, cm.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !cm.Optional {
			return false, true, err.Error()
		}
		existing, err := c.configMapGetter.ConfigMaps(c.targetNamespace).Get(ctx, nameFor(cm.Name, revision), metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !cm.Optional {
			return false, false, err.Error()
		}
		if required != nil {
			requiredData = required.Data
		}
		if existing != nil {
			existingData = existing.Data
		}
		if !equality.Semantic.DeepEqual(existingData, requiredData) {
			if klog.V(4).Enabled() {
				klog.Infof("configmap %q changes for revision %d: %s", cm.Name, revision, resourceapply.JSONPatchNoError(existing, required))
			}
			// "configmap/foo has changed" when there is actual change in data
			// "configmap/foo has been created" when the existing configmap was empty (iow. the configmap is optional)
			verb := "changed"
			if len(existingData) == 0 {
				verb = "been created"
			}
			req := "required"
			if cm.Optional {
				req = "optional"
			}
			configChanges = append(configChanges, fmt.Sprintf("%s configmap/%s has %s", req, cm.Name, verb))
		}
	}

	secretChanges := []string{}
	for _, s := range c.secrets {
		requiredData := map[string][]byte{}
		existingData := map[string][]byte{}

		required, err := c.secretGetter.Secrets(c.targetNamespace).Get(ctx, s.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !s.Optional {
			return false, true, err.Error()
		}
		existing, err := c.secretGetter.Secrets(c.targetNamespace).Get(ctx, nameFor(s.Name, revision), metav1.GetOptions{})
		if apierrors.IsNotFound(err) && !s.Optional {
			return false, false, err.Error()
		}
		if required != nil {
			requiredData = required.Data
		}
		if existing != nil {
			existingData = existing.Data
		}
		if !equality.Semantic.DeepEqual(existingData, requiredData) {
			if klog.V(4).Enabled() {
				klog.Infof("Secret %q changes for revision %d: %s", s.Name, revision, resourceapply.JSONPatchSecretNoError(existing, required))
			}
			// "configmap/foo has changed" when there is actual change in data
			// "configmap/foo has been created" when the existing configmap was empty (iow. the configmap is optional)
			verb := "changed"
			if len(existingData) == 0 {
				verb = "been created"
			}
			req := "required"
			if s.Optional {
				req = "optional"
			}
			secretChanges = append(secretChanges, fmt.Sprintf("%s secret/%s has %s", req, s.Name, verb))
		}
	}

	if len(secretChanges) > 0 || len(configChanges) > 0 {
		return false, false, strings.Join(append(secretChanges, configChanges...), ",")
	}

	return true, false, ""
}

// returns true if we created a revision
func (c RevisionController) createNewRevision(ctx context.Context, recorder events.Recorder, revision int32, reason string) (bool, error) {
	// Create a new InProgress status configmap
	desiredStatusConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.targetNamespace,
			Name:      nameFor("revision-status", revision),
			Annotations: map[string]string{
				"operator.openshift.io/revision-ready": "false",
			},
		},
		Data: map[string]string{
			"revision": fmt.Sprintf("%d", revision),
			"reason":   reason,
		},
	}
	createdStatus, err := c.configMapGetter.ConfigMaps(desiredStatusConfigMap.Namespace).Create(ctx, desiredStatusConfigMap, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		if createdStatus == nil || len(createdStatus.UID) == 0 {
			createdStatus, err = c.configMapGetter.ConfigMaps(desiredStatusConfigMap.Namespace).Get(ctx, desiredStatusConfigMap.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
		}
		// take a live GET here to get current status to check the annotation
		if createdStatus.Annotations["operator.openshift.io/revision-ready"] == "true" {
			// no work to do because our cache is out of date and when we're updated, we will be able to see the result
			klog.Infof("down the branch indicating that our cache was out of date and we're trying to recreate a revision.")
			return false, nil
		}
		// update the sync and continue
	case err != nil:
		return false, err
	}

	ownerRefs := []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       createdStatus.Name,
		UID:        createdStatus.UID,
	}}

	for _, cm := range c.configMaps {
		obj, _, err := resourceapply.SyncConfigMap(ctx, c.configMapGetter, recorder, c.targetNamespace, cm.Name, c.targetNamespace, nameFor(cm.Name, revision), ownerRefs)
		if err != nil {
			return false, err
		}
		if obj == nil && !cm.Optional {
			return false, apierrors.NewNotFound(corev1.Resource("configmaps"), cm.Name)
		}
	}
	for _, s := range c.secrets {
		obj, _, err := resourceapply.SyncSecret(ctx, c.secretGetter, recorder, c.targetNamespace, s.Name, c.targetNamespace, nameFor(s.Name, revision), ownerRefs)
		if err != nil {
			return false, err
		}
		if obj == nil && !s.Optional {
			return false, apierrors.NewNotFound(corev1.Resource("secrets"), s.Name)
		}
	}

	createdStatus.Annotations["operator.openshift.io/revision-ready"] = "true"
	if _, err := c.configMapGetter.ConfigMaps(createdStatus.Namespace).Update(ctx, createdStatus, metav1.UpdateOptions{}); err != nil {
		return false, err
	}

	return true, nil
}

// getLatestAvailableRevision returns the latest known revision to the operator
// This is determined by checking revision status configmaps.
func (c RevisionController) getLatestAvailableRevision(ctx context.Context) (int32, error) {
	// this appears to use a cached getter.  I conceded that past-David should have explicitly used Listers
	configMaps, err := c.configMapGetter.ConfigMaps(c.targetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	var latestRevision int32
	for _, configMap := range configMaps.Items {
		if !strings.HasPrefix(configMap.Name, "revision-status-") {
			continue
		}
		if revision, ok := configMap.Data["revision"]; ok {
			revisionNumber, err := strconv.Atoi(revision)
			if err != nil {
				return 0, err
			}
			if int32(revisionNumber) > latestRevision {
				latestRevision = int32(revisionNumber)
			}
		}
	}
	// If there are no configmaps, then this should actually be revision 0
	return latestRevision, nil
}

func (c RevisionController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, _, latestAvailableRevisionSeenByOperator, resourceVersion, err := c.operatorClient.GetLatestRevisionState()
	if err != nil {
		return err
	}

	if !management.IsOperatorManaged(operatorSpec.ManagementState) {
		return nil
	}

	if shouldCreateNewRevision, err := c.revisionPrecondition(ctx); err != nil || !shouldCreateNewRevision {
		return err
	}

	// If the operator status's latest available revision is not the same as the observed latest revision, update the operator.
	latestObservedRevision, err := c.getLatestAvailableRevision(ctx)
	if err != nil {
		return err
	}
	if latestObservedRevision != 0 && latestAvailableRevisionSeenByOperator != latestObservedRevision {
		// Then make sure that revision number is what's in the operator status
		_, _, err := c.operatorClient.UpdateLatestRevisionOperatorStatus(ctx, latestObservedRevision)
		if err != nil {
			return err
		}
		// regardless of whether we made a change, requeue to rerun the sync with updated status
		return factory.SyntheticRequeueError
	}

	requeue, syncErr := c.createRevisionIfNeeded(ctx, syncCtx.Recorder(), latestAvailableRevisionSeenByOperator, resourceVersion)
	if requeue && syncErr == nil {
		return factory.SyntheticRequeueError
	}
	err = syncErr

	// update failing condition
	cond := operatorv1.OperatorCondition{
		Type:   condition.RevisionControllerDegradedConditionType,
		Status: operatorv1.ConditionFalse,
	}
	if err != nil {
		cond.Status = operatorv1.ConditionTrue
		cond.Reason = "Error"
		cond.Message = err.Error()
	}
	if _, _, updateError := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(cond)); updateError != nil {
		if err == nil {
			return updateError
		}
	}

	return err
}

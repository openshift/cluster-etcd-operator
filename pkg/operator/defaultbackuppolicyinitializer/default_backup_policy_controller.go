package defaultbackuppolicyinitializer

import (
	"context"
	"fmt"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultBackupPolicyInitializedCondition records that the CEO has made its
	// one-time decision about the default backup policy. It does not mean that
	// the policy still exists or must continue to be reconciled.
	DefaultBackupPolicyInitializedCondition = "DefaultBackupPolicyInitialized"

	defaultBackupPolicyCreatedReason              = "DefaultPolicyCreated"
	defaultBackupPolicyInitializationFailedReason = "InitializationFailed"
	existingDefaultBackupPolicyReason             = "ExistingDefaultPolicyDetected"
	defaultPolicyRestoredReason                   = "DefaultPolicyRestored"
	defaultBackupPolicyManifestAsset              = "etcd/default-backup-policy.yaml"
	defaultPolicyName                             = "default"
)

type defaultBackupPolicyInitController struct {
	operatorClient     v1helpers.StaticPodOperatorClient
	backupPolicyClient operatorv1alpha1client.EtcdBackupPolicyInterface
	stop               context.CancelFunc
}

func NewDefaultBackupPolicyInitController(
	operatorClient v1helpers.StaticPodOperatorClient,
	backupPolicyClient operatorv1alpha1client.EtcdBackupPolicyInterface,
	backupPolicyInformer cache.SharedIndexInformer,
	recorder events.Recorder,
	stop context.CancelFunc,
) factory.Controller {

	c := &defaultBackupPolicyInitController{
		operatorClient:     operatorClient,
		backupPolicyClient: backupPolicyClient,
		stop:               stop,
	}

	return factory.New().
		WithInformers(operatorClient.Informer(), backupPolicyInformer).
		WithSync(c.sync).
		ToController("DefaultBackupPolicyInitializerController", recorder)
}

func (c *defaultBackupPolicyInitController) sync(
	ctx context.Context,
	_ factory.SyncContext,
) error {

	err := c.initialize(ctx)

	if err != nil {
		return err
	}

	c.stop()
	return nil
}

// initialize performs the one-time default policy
// initialization. It intentionally does not reconcile the policy after the
// initialization marker has been recorded, so an administrator can edit or
// delete it without the CEO restoring it.
func (c *defaultBackupPolicyInitController) initialize(ctx context.Context) error {
	_, status, _, err := c.operatorClient.GetStaticPodOperatorStateWithQuorum(ctx)
	if err != nil {
		return fmt.Errorf("failed to get Etcd operator state: %w", err)
	}

	condition := v1helpers.FindOperatorCondition(status.Conditions, DefaultBackupPolicyInitializedCondition)

	var reason string

	switch {
	case condition == nil:
		reason, err = c.ensureDefaultBackupPolicy(ctx)
	case condition.Status == operatorv1.ConditionTrue:
		return nil
	case condition.Status == operatorv1.ConditionFalse:
		reason, err = c.ensureDefaultBackupPolicy(ctx)
	default:
		return fmt.Errorf("unexpected initialization condition status: %s", condition.Status)
	}

	if err != nil {
		// If condition is nil what is given to the function recordInitializationFailure?
		errRecordingFailure := c.recordInitilaizationFailure(ctx, err)
		if errRecordingFailure != nil {
			return errRecordingFailure
		}
		return err
	}

	_, _, err = v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    DefaultBackupPolicyInitializedCondition,
		Status:  operatorv1.ConditionTrue,
		Reason:  reason,
		Message: "",
	}))
	if err != nil {
		return fmt.Errorf("failed to record default backup policy initialization: %w", err)
	}

	return nil
}

func (c *defaultBackupPolicyInitController) recordInitilaizationFailure(ctx context.Context, initErr error) error {
	_, _, err := v1helpers.UpdateStatus(
		ctx, c.operatorClient,
		func(status *operatorv1.OperatorStatus) error {
			if v1helpers.IsOperatorConditionTrue(
				status.Conditions,
				DefaultBackupPolicyInitializedCondition,
			) {
				return nil
			}

			v1helpers.SetOperatorCondition(&status.Conditions, operatorv1.OperatorCondition{
				Type: DefaultBackupPolicyInitializedCondition,
				Status: operatorv1.ConditionFalse,
				Reason: defaultBackupPolicyInitializationFailedReason,
				Message: initErr.Error(),
			})
			return nil
		})
	
	return err

}

func (c *defaultBackupPolicyInitController) ensureDefaultBackupPolicy(ctx context.Context) (string, error) {
	defaultPolicy, err := c.backupPolicyClient.Get(ctx, defaultPolicyName, metav1.GetOptions{})

	if err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}

		// No policy found
		err = c.createDefaultBackupPolicy(ctx)

		if err != nil {
			return "", err
		}

		return defaultBackupPolicyCreatedReason, nil
	}

	err = c.resetDefaultBackupPolicy(ctx, defaultPolicy)

	if err != nil {
		return "", err
	}

	return defaultPolicyRestoredReason, nil
}

func (c *defaultBackupPolicyInitController) createDefaultBackupPolicy(ctx context.Context) error {
	defaultPolicy, err := decodeDefaultBackupPolicy()
	if err != nil {
		return err
	}

	_, err = c.backupPolicyClient.Create(ctx, defaultPolicy, metav1.CreateOptions{})

	if err != nil {
		return err
	}

	return nil
}

func (c *defaultBackupPolicyInitController) resetDefaultBackupPolicy(ctx context.Context, existingDefaultPolicy *operatorv1alpha1.EtcdBackupPolicy) error {
	desiredDefaultPolicy, err := decodeDefaultBackupPolicy()

	if err != nil {
		return err
	}

	updatedDefaultPolicy := existingDefaultPolicy.DeepCopy()
	updatedDefaultPolicy.Spec = *desiredDefaultPolicy.Spec.DeepCopy()

	_, err = c.backupPolicyClient.Update(
		ctx,
		updatedDefaultPolicy,
		metav1.UpdateOptions{},
	)

	if err != nil {
		return err
	}

	return nil
}

func decodeDefaultBackupPolicy() (*operatorv1alpha1.EtcdBackupPolicy, error) {
	scheme := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add EtcdBackupPolicy scheme: %w", err)
	}

	codec := serializer.NewCodecFactory(scheme)
	obj, err := runtime.Decode(codec.UniversalDecoder(operatorv1alpha1.GroupVersion), bindata.MustAsset(defaultBackupPolicyManifestAsset))
	if err != nil {
		return nil, fmt.Errorf("failed to decode default EtcdBackupPolicy manifest: %w", err)
	}

	policy, ok := obj.(*operatorv1alpha1.EtcdBackupPolicy)
	if !ok {
		return nil, fmt.Errorf("default backup policy manifest decoded as %T", obj)
	}

	return policy, nil
}

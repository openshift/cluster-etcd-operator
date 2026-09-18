package defaultbackuppolicyinitializer

import (
	"context"
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

const (
	// DefaultBackupPolicyInitializedCondition records that the CEO has made its
	// one-time decision about the default backup policy. It does not mean that
	// the policy still exists or must continue to be reconciled.
	DefaultBackupPolicyInitializedCondition = "DefaultBackupPolicyInitialized"

	defaultBackupPolicyCreatedReason = "DefaultPolicyCreated"
	existingBackupPolicyReason       = "ExistingPolicyDetected"
	defaultBackupPolicyManifestAsset = "etcd/default-backup-policy.yaml"
)

// InitializeDefaultBackupPolicy performs the one-time default policy
// initialization. It intentionally does not reconcile the policy after the
// initialization marker has been recorded, so an administrator can edit or
// delete it without the CEO restoring it.
func InitializeDefaultBackupPolicy(
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	backupPolicyClient operatorv1alpha1client.EtcdBackupPolicyInterface,
) error {
	_, status, _, err := operatorClient.GetStaticPodOperatorStateWithQuorum(ctx)
	if err != nil {
		return fmt.Errorf("failed to get Etcd operator state: %w", err)
	}

	// Check if the marker for default backup policy initialized exists and it is true
	if status != nil && v1helpers.IsOperatorConditionTrue(status.Conditions, DefaultBackupPolicyInitializedCondition) {
		return nil
	}

	// List all the exisiting backup policies
	policies, err := backupPolicyClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list EtcdBackupPolicies: %w", err)
	}

	reason := existingBackupPolicyReason
	// Check if there's already any backup policy
	if len(policies.Items) == 0 {
		policy, err := decodeDefaultBackupPolicy()
		if err != nil {
			return err
		}

		if _, err := backupPolicyClient.Create(ctx, policy, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create default EtcdBackupPolicy: %w", err)
		}
		reason = defaultBackupPolicyCreatedReason
	}

	_, _, err = v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    DefaultBackupPolicyInitializedCondition,
		Status:  operatorv1.ConditionTrue,
		Reason:  reason,
		Message: "The initial EtcdBackupPolicy decision has been completed.",
	}))
	if err != nil {
		return fmt.Errorf("failed to record default backup policy initialization: %w", err)
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

package defaultbackuppolicyinitializer

import (
	"errors"
	"reflect"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

// TestDecodeDefaultBackupPolicy verifies the settings in the embedded default policy manifest.
func TestDecodeDefaultBackupPolicy(t *testing.T) {
	defaultPolicy, err := decodeDefaultBackupPolicy()

	if err != nil {
		t.Fatal(err)
	}

	if defaultPolicy == nil {
		t.Fatalf("default backup policy: it was nil")
	}

	if defaultPolicy.Name != "default" {
		t.Fatalf("default backup policy: name is %s, expected %s", defaultPolicy.Name, "default")
	}

	if defaultPolicy.Spec.Schedule != "@daily" {
		t.Fatalf("default backup policy: schedule is %s, expected %s", defaultPolicy.Spec.Schedule, "@daily")
	}

	if defaultPolicy.Spec.FailedBackupsHistoryLimit == nil {
		t.Fatalf("default backup policy: failed backup history limit is nil, expected %d", 1)
	}

	if *defaultPolicy.Spec.FailedBackupsHistoryLimit != 1 {
		t.Fatalf("default backup policy: failed backup history limit is %d, expected %d", *defaultPolicy.Spec.FailedBackupsHistoryLimit, 1)
	}

	if len(defaultPolicy.Spec.RetentionRules) != 1 {
		t.Fatalf("default backup policy: retention rules items number is %d, expected %d", len(defaultPolicy.Spec.RetentionRules), 1)
	}

	if defaultPolicy.Spec.RetentionRules[0].Type != operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity {
		t.Fatalf("default backup policy: retention rules type is %s, expected %s", defaultPolicy.Spec.RetentionRules[0].Type, operatorv1alpha1.EtcdBackupPolicyRetentionRuleMaxQuantity)
	}

	if defaultPolicy.Spec.RetentionRules[0].MaxQuantity != 2 {
		t.Fatalf("default backup policy: retention rules max quantity is %d, expected %d", defaultPolicy.Spec.RetentionRules[0].MaxQuantity, 2)
	}

	if defaultPolicy.Spec.Storage.Type != operatorv1alpha1.EtcdBackupStorageTypeLocal {
		t.Fatalf("default backup policy: storage type is %s, expected %s", defaultPolicy.Spec.Storage.Type, operatorv1alpha1.EtcdBackupStorageTypeLocal)
	}

	if defaultPolicy.Spec.Storage.Local.HostPath != "/etc/kubernetes/cluster-backup" {
		t.Fatalf("default backup policy: storage local host path is %s, expected %s", defaultPolicy.Spec.Storage.Local.HostPath, "/etc/kubernetes/cluster-backup")
	}
}

func newInitTestController(t *testing.T, conditions []operatorv1.OperatorCondition, policies ...runtime.Object) (*defaultBackupPolicyInitController, *operatorfake.Clientset) {
	t.Helper()
	operatorFake := operatorfake.NewSimpleClientset(policies...)
	return &defaultBackupPolicyInitController{
		operatorClient:     testutils.FakeStaticPodOperatorClient(t, conditions),
		backupPolicyClient: operatorFake.OperatorV1alpha1().EtcdBackupPolicies(),
	}, operatorFake
}

func requireInitCondition(t *testing.T, controller *defaultBackupPolicyInitController, expectedStatus operatorv1.ConditionStatus, expectedReason string) {
	t.Helper()
	_, status, _, err := controller.operatorClient.GetStaticPodOperatorStateWithQuorum(t.Context())
	if err != nil {
		t.Fatalf("get Etcd operator state: %v", err)
	}
	condition := v1helpers.FindOperatorCondition(status.Conditions, DefaultBackupPolicyInitializedCondition)
	if condition == nil {
		t.Fatal("default backup policy initialization condition was not recorded")
	}
	if condition.Status != expectedStatus || condition.Reason != expectedReason {
		t.Fatalf("initialization condition is %s/%s, expected %s/%s", condition.Status, condition.Reason, expectedStatus, expectedReason)
	}
}

func countPolicyActions(client *operatorfake.Clientset, verb string) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "etcdbackuppolicies" && action.GetVerb() == verb {
			count++
		}
	}
	return count
}

// TestInitializeCreatesDefaultPolicy verifies first-time creation and the persistent success marker.
func TestInitializeCreatesDefaultPolicy(t *testing.T) {
	controller, client := newInitTestController(t, nil)
	if err := controller.initialize(t.Context()); err != nil {
		t.Fatalf("initialize default backup policy: %v", err)
	}

	policy, err := controller.backupPolicyClient.Get(t.Context(), defaultPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created default backup policy: %v", err)
	}
	if policy == nil || policy.Name != defaultPolicyName {
		t.Fatalf("created policy is %v, expected %q", policy, defaultPolicyName)
	}
	requireInitCondition(t, controller, operatorv1.ConditionTrue, defaultBackupPolicyCreatedReason)
	if got := countPolicyActions(client, "create"); got != 1 {
		t.Fatalf("created %d policies, expected one", got)
	}
}

// TestInitializeReconcilesDefaultPolicyBeforeCompletion verifies that the default policy is restored until initialization completes.
func TestInitializeReconcilesDefaultPolicyBeforeCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                string
		policy              *operatorv1alpha1.EtcdBackupPolicy
		conditions          []operatorv1.OperatorCondition
		expectedReason      string
		expectedCreateCount int
		expectedUpdateCount int
	}{
		{name: "default policy", policy: testutils.FakeEtcdBackupPolicy(defaultPolicyName, "@hourly"), expectedReason: defaultPolicyRestoredReason, expectedUpdateCount: 1},
		{name: "another policy", policy: testutils.FakeEtcdBackupPolicy("custom", "@hourly"), expectedReason: defaultBackupPolicyCreatedReason, expectedCreateCount: 1},
		{name: "retry with default policy", policy: testutils.FakeEtcdBackupPolicy(defaultPolicyName, "@hourly"), conditions: []operatorv1.OperatorCondition{{Type: DefaultBackupPolicyInitializedCondition, Status: operatorv1.ConditionFalse}}, expectedReason: defaultPolicyRestoredReason, expectedUpdateCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.policy.DeepCopy()
			controller, client := newInitTestController(t, tc.conditions, tc.policy)
			if err := controller.initialize(t.Context()); err != nil {
				t.Fatalf("initialize with existing policy: %v", err)
			}
			if got := countPolicyActions(client, "create"); got != tc.expectedCreateCount {
				t.Fatalf("created %d policies, expected %d", got, tc.expectedCreateCount)
			}
			if got := countPolicyActions(client, "update"); got != tc.expectedUpdateCount {
				t.Fatalf("updated %d policies, expected %d", got, tc.expectedUpdateCount)
			}

			after, err := controller.backupPolicyClient.Get(t.Context(), before.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get existing policy: %v", err)
			}
			if before.Name != defaultPolicyName && !reflect.DeepEqual(after.Spec, before.Spec) {
				t.Fatalf("custom policy spec changed: got %+v, expected %+v", after.Spec, before.Spec)
			}

			defaultPolicy, err := controller.backupPolicyClient.Get(t.Context(), defaultPolicyName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get default policy: %v", err)
			}
			expectedDefaultPolicy, err := decodeDefaultBackupPolicy()
			if err != nil {
				t.Fatalf("decode expected default policy: %v", err)
			}
			if !reflect.DeepEqual(defaultPolicy.Spec, expectedDefaultPolicy.Spec) {
				t.Fatalf("default policy spec is %+v, expected %+v", defaultPolicy.Spec, expectedDefaultPolicy.Spec)
			}
			requireInitCondition(t, controller, operatorv1.ConditionTrue, tc.expectedReason)
		})
	}
}

// TestInitializeDoesNotReconcileAfterCompletion verifies that administrator edits and deletion persist after initialization.
func TestInitializeDoesNotReconcileAfterCompletion(t *testing.T) {
	conditions := []operatorv1.OperatorCondition{{Type: DefaultBackupPolicyInitializedCondition, Status: operatorv1.ConditionTrue, Reason: defaultBackupPolicyCreatedReason}}
	t.Run("edited policy", func(t *testing.T) {
		existing := testutils.FakeEtcdBackupPolicy(defaultPolicyName, "@hourly")
		controller, client := newInitTestController(t, conditions, existing)
		if err := controller.initialize(t.Context()); err != nil {
			t.Fatalf("initialize completed policy: %v", err)
		}
		if got := len(client.Actions()); got != 0 {
			t.Fatalf("made %d policy API calls after completion, expected none", got)
		}
		policy, err := controller.backupPolicyClient.Get(t.Context(), defaultPolicyName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get edited policy: %v", err)
		}
		if policy == nil || !reflect.DeepEqual(policy.Spec, existing.Spec) {
			t.Fatalf("edited policy changed: policy=%v", policy)
		}
	})
	t.Run("deleted policy", func(t *testing.T) {
		controller, client := newInitTestController(t, conditions)
		if err := controller.initialize(t.Context()); err != nil {
			t.Fatalf("initialize completed policy: %v", err)
		}
		if got := len(client.Actions()); got != 0 {
			t.Fatalf("made %d policy API calls after completion, expected none", got)
		}
		_, err := controller.backupPolicyClient.Get(t.Context(), defaultPolicyName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("deleted policy was recreated; get error: %v", err)
		}
	})
}

// TestInitializeRetriesAfterGetFailure verifies a failed API read records False and succeeds on retry.
func TestInitializeRetriesAfterGetFailure(t *testing.T) {
	controller, client := newInitTestController(t, nil)
	getErr := errors.New("get temporarily unavailable")
	failFirst := true
	client.PrependReactor("get", "etcdbackuppolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failFirst {
			failFirst = false
			return true, nil, getErr
		}
		return false, nil, nil
	})
	if err := controller.initialize(t.Context()); !errors.Is(err, getErr) {
		t.Fatalf("first initialization error is %v, expected %v", err, getErr)
	}
	requireInitCondition(t, controller, operatorv1.ConditionFalse, defaultBackupPolicyInitializationFailedReason)
	if got := countPolicyActions(client, "create"); got != 0 {
		t.Fatalf("created %d policies after get failure, expected none", got)
	}
	if err := controller.initialize(t.Context()); err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
	requireInitCondition(t, controller, operatorv1.ConditionTrue, defaultBackupPolicyCreatedReason)
	if got := countPolicyActions(client, "create"); got != 1 {
		t.Fatalf("created %d policies after retry, expected one", got)
	}
}

// TestInitializeRetriesAfterCreateFailure verifies a failed create leaves the marker False until retry succeeds.
func TestInitializeRetriesAfterCreateFailure(t *testing.T) {
	controller, client := newInitTestController(t, nil)
	createErr := errors.New("create temporarily unavailable")
	failFirst := true
	client.PrependReactor("create", "etcdbackuppolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failFirst {
			failFirst = false
			return true, nil, createErr
		}
		return false, nil, nil
	})
	if err := controller.initialize(t.Context()); !errors.Is(err, createErr) {
		t.Fatalf("first initialization error is %v, expected %v", err, createErr)
	}
	requireInitCondition(t, controller, operatorv1.ConditionFalse, defaultBackupPolicyInitializationFailedReason)
	if err := controller.initialize(t.Context()); err != nil {
		t.Fatalf("retry initialization: %v", err)
	}
	requireInitCondition(t, controller, operatorv1.ConditionTrue, defaultBackupPolicyCreatedReason)
	if got := countPolicyActions(client, "create"); got != 2 {
		t.Fatalf("attempted %d policy creates, expected two", got)
	}
}

// TestInitializePreservesCreateErrorWhenStatusUpdateFails verifies that a failed status update does not replace the original initialization error.
func TestInitializePreservesCreateErrorWhenStatusUpdateFails(t *testing.T) {
	controller, client := newInitTestController(t, nil)
	createErr := errors.New("create temporarily unavailable")
	statusErr := errors.New("status temporarily unavailable")
	client.PrependReactor("create", "etcdbackuppolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, createErr
	})

	statusUpdateAttempted := false
	controller.operatorClient = v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{},
		func(_ string, status *operatorv1.StaticPodOperatorStatus) error {
			statusUpdateAttempted = true
			condition := v1helpers.FindOperatorCondition(status.Conditions, DefaultBackupPolicyInitializedCondition)
			if condition == nil {
				t.Fatal("status update did not include the initialization condition")
			}
			if condition.Status != operatorv1.ConditionFalse || condition.Reason != defaultBackupPolicyInitializationFailedReason || condition.Message != createErr.Error() {
				t.Fatalf("unexpected initialization failure condition: %+v", condition)
			}
			return statusErr
		}, nil,
	)

	if err := controller.initialize(t.Context()); !errors.Is(err, createErr) {
		t.Fatalf("initialization error is %v, expected the original create error %v", err, createErr)
	}
	if !statusUpdateAttempted {
		t.Fatal("initialization did not attempt to record the failure")
	}
}

// TestInitializeRetriesWhenCreateAlreadyExists verifies a concurrently created default policy is reconciled on retry.
func TestInitializeRetriesWhenCreateAlreadyExists(t *testing.T) {
	controller, client := newInitTestController(t, nil)
	existing := testutils.FakeEtcdBackupPolicy(defaultPolicyName, "@hourly")
	firstCreate := true
	client.PrependReactor("create", "etcdbackuppolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !firstCreate {
			return false, nil, nil
		}
		firstCreate = false
		if err := client.Tracker().Add(existing); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: "operator.openshift.io", Resource: "etcdbackuppolicies"}, defaultPolicyName)
	})
	if err := controller.initialize(t.Context()); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("first initialization error is %v, expected AlreadyExists", err)
	}
	requireInitCondition(t, controller, operatorv1.ConditionFalse, defaultBackupPolicyInitializationFailedReason)
	if err := controller.initialize(t.Context()); err != nil {
		t.Fatalf("retry after concurrent create: %v", err)
	}
	requireInitCondition(t, controller, operatorv1.ConditionTrue, defaultPolicyRestoredReason)
	policy, err := controller.backupPolicyClient.Get(t.Context(), defaultPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get concurrently created policy: %v", err)
	}
	expectedDefaultPolicy, err := decodeDefaultBackupPolicy()
	if err != nil {
		t.Fatalf("decode expected default policy: %v", err)
	}
	if policy == nil || !reflect.DeepEqual(policy.Spec, expectedDefaultPolicy.Spec) {
		t.Fatalf("concurrently created policy was not restored: policy=%v", policy)
	}
	if got := countPolicyActions(client, "create"); got != 1 {
		t.Fatalf("attempted %d policy creates, expected one", got)
	}
	if got := countPolicyActions(client, "update"); got != 1 {
		t.Fatalf("updated concurrently created policy %d times, expected one", got)
	}
}

// TestInitializeRetriesAfterStatusFailure verifies a retry restores the default policy without creating a duplicate after status recording fails.
func TestInitializeRetriesAfterStatusFailure(t *testing.T) {
	controller, client := newInitTestController(t, nil)
	statusErr := errors.New("status temporarily unavailable")
	failFirst := true
	controller.operatorClient = v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{},
		func(string, *operatorv1.StaticPodOperatorStatus) error {
			if failFirst {
				failFirst = false
				return statusErr
			}
			return nil
		}, nil,
	)
	if err := controller.initialize(t.Context()); !errors.Is(err, statusErr) {
		t.Fatalf("first initialization error is %v, expected %v", err, statusErr)
	}
	_, status, _, err := controller.operatorClient.GetStaticPodOperatorStateWithQuorum(t.Context())
	if err != nil {
		t.Fatalf("get Etcd operator state after failed status update: %v", err)
	}
	if condition := v1helpers.FindOperatorCondition(status.Conditions, DefaultBackupPolicyInitializedCondition); condition != nil {
		t.Fatalf("initialization was recorded despite status update failure: %+v", condition)
	}
	if got := countPolicyActions(client, "create"); got != 1 {
		t.Fatalf("created %d policies, expected one", got)
	}
	if err := controller.initialize(t.Context()); err != nil {
		t.Fatalf("retry after status failure: %v", err)
	}
	requireInitCondition(t, controller, operatorv1.ConditionTrue, defaultPolicyRestoredReason)
	if got := countPolicyActions(client, "create"); got != 1 {
		t.Fatalf("created %d policies after retry, expected no duplicate", got)
	}
	if got := countPolicyActions(client, "update"); got != 1 {
		t.Fatalf("updated %d policies after retry, expected one restore", got)
	}
}

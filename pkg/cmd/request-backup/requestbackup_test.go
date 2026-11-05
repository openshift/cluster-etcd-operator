package requestbackup

import (
	"context"
	"errors"
	"testing"
	"time"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	testingclock "k8s.io/utils/clock/testing"
)

type testCaseRequestBackup struct {
	opts           requestBackupOpts
	backupPolicies []*operatorv1alpha1.EtcdBackupPolicy
	backups        []*operatorv1alpha1.EtcdBackup
	nodes          []*corev1.Node
	expectError    bool
	setup          func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
	validate       func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
}

func runRequestBackupTest(t *testing.T, tc testCaseRequestBackup) {
	t.Helper()
	err := tc.opts.Validate()
	require.NoError(t, err)

	operatorObjs := make([]runtime.Object, 0, len(tc.backupPolicies)+len(tc.backups))
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backupPolicies)
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backups)

	k8sObjs := make([]runtime.Object, 0, len(tc.nodes))
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.nodes)

	client := k8sfakeclient.NewSimpleClientset(k8sObjs...)
	operatorFake := operatorfake.NewSimpleClientset(operatorObjs...)

	sharedFactory := informers.NewSharedInformerFactory(client, 0)
	operatorSharedFactory := operatorinformers.NewSharedInformerFactory(operatorFake, 0)

	backupsInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackups()
	backupPoliciesInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackupPolicies()
	nodesInformer := sharedFactory.Core().V1().Nodes()

	backupsInformerHasSynced := backupsInformer.Informer().HasSynced
	backupPoliciesInformerHasSynced := backupPoliciesInformer.Informer().HasSynced
	nodesInformerHasSynced := nodesInformer.Informer().HasSynced

	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), nodesInformerHasSynced, backupsInformerHasSynced, backupPoliciesInformerHasSynced)

	fakeClock := testingclock.NewFakeClock(time.Now())
	eventRecorder := events.NewInMemoryRecorder("test", fakeClock)
	t.Cleanup(eventRecorder.Shutdown)

	if tc.setup != nil {
		tc.setup(t, client, operatorFake)
	}

	err = tc.opts.requestBackups(ctx, client, operatorFake.OperatorV1alpha1())
	if tc.expectError {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}

	if tc.validate != nil {
		tc.validate(t, client, operatorFake)
	}
}

func TestRequestBackupForPolicy(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		runRequestBackupTest(t, testCaseRequestBackup{
			opts: requestBackupOpts{
				etcdBackupPolicyName: "test-backup-policy",
			},
			backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
				testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour), func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
					backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
						Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
						Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
							EtcdBackupStorageLocal: operatorv1alpha1.EtcdBackupStorageLocal{
								HostPath: "/etc/etcdbackups",
							},
						},
					}
				})},
			nodes: []*corev1.Node{
				testutils.FakeNode("test-node-2", testutils.WithMasterLabel()),
				testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				// Verify new backup was created (nodeCount = 1 by default)
				backups, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(context.TODO(), v1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, backups.Items, 1)

				backup := backups.Items[0]
				require.Equal(t, "test-backup-policy", backup.Labels[backuphelpers.LabelEtcdBackupPolicy])
				// Local backup is assigned to a specific node. With no previous backups, round robin starts at the first node.
				require.Equal(t, "test-node-1", backup.Spec.NodeSelector[corev1.LabelHostname])
			},
		})
	})
	t.Run("pvc", func(t *testing.T) {
		runRequestBackupTest(t, testCaseRequestBackup{
			opts: requestBackupOpts{
				etcdBackupPolicyName: "test-backup-policy",
			},
			backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
				testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))},
			nodes: []*corev1.Node{
				testutils.FakeNode("test-node-2", testutils.WithMasterLabel()),
				testutils.FakeNode("test-node-1", testutils.WithMasterLabel())},
			validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
				// Verify new backup was created
				backups, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(context.TODO(), v1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, backups.Items, 1)

				backup := backups.Items[0]
				require.Equal(t, "test-backup-policy", backup.Labels[backuphelpers.LabelEtcdBackupPolicy])
				// PVC backups are not assigned to a specific node to allow for the scheduler to handle volume topology
				require.NotContains(t, backup.Spec.NodeSelector, corev1.LabelHostname)
			},
		})
	})
}

func TestRequestBackupForPolicyMultipleLocalBackupsWithSelector(t *testing.T) {
	withSpecialLabel := func(node *corev1.Node) {
		node.Labels["special"] = "label"
	}
	runRequestBackupTest(t, testCaseRequestBackup{
		opts: requestBackupOpts{
			etcdBackupPolicyName: "test-backup-policy",
		},
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour), func(backup *operatorv1alpha1.EtcdBackupPolicy) {
				backup.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
					Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
						EtcdBackupStorageLocal: operatorv1alpha1.EtcdBackupStorageLocal{
							HostPath: "/etc/backups",
						},
						Distribution: operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes,
					},
				}
				backup.Spec.NodeSelector = map[string]string{"special": "label"}
			})},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel(), withSpecialLabel),
			testutils.FakeNode("test-node-3", testutils.WithMasterLabel(), withSpecialLabel)},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Verify new backups were created
			backups, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(context.TODO(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backups.Items, 2)

			expectedNodes := []string{"test-node-2", "test-node-3"}
			backupNodes := make([]string, 2)
			for i, backup := range backups.Items {
				require.Equal(t, "test-backup-policy", backup.Labels[backuphelpers.LabelEtcdBackupPolicy])
				require.NotNil(t, backup.Spec.NodeSelector)
				backupNodes[i] = backup.Spec.NodeSelector[corev1.LabelHostname]
			}
			require.ElementsMatch(t, backupNodes, expectedNodes)
		},
	})
}

func TestRequestBackupForPolicyWithActiveBackup(t *testing.T) {
	runRequestBackupTest(t, testCaseRequestBackup{
		opts: requestBackupOpts{
			etcdBackupPolicyName: "test-backup-policy",
		},
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))},
		backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup("test-backup", testutils.WithBackupPolicy("test-backup-policy"))},
		nodes:   []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetAction[k8stesting.CreateActionImpl](operatorFake.Fake.Actions())
			require.False(t, ok, "expected no create action")

			backups, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(context.TODO(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backups.Items, 1)
			require.Equal(t, backups.Items[0].Name, "test-backup")
		},
	})
}

func TestRequestBackupForPolicyWithFinishedBackups(t *testing.T) {
	runRequestBackupTest(t, testCaseRequestBackup{
		opts: requestBackupOpts{
			etcdBackupPolicyName: "test-backup-policy",
		},
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-completed", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupCompleted()),
			testutils.FakeEtcdBackup("test-backup-failed", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed)),
		},
		nodes: []*corev1.Node{testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			action, ok := testutils.GetAction[k8stesting.CreateActionImpl](operatorFake.Fake.Actions())
			require.True(t, ok, "expected create action")
			backup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
			require.True(t, ok, "expected EtcdBackup to be created")

			require.Equal(t, "test-backup-policy", backup.Labels[backuphelpers.LabelEtcdBackupPolicy])
		},
	})
}

func TestRequestBackupForDeletedPolicy(t *testing.T) {
	runRequestBackupTest(t, testCaseRequestBackup{
		opts: requestBackupOpts{
			etcdBackupPolicyName: "policy-not-found",
		},
		expectError: true,
	})
}

func TestRequestBackupForPolicyWithAssets(t *testing.T) {
	runRequestBackupTest(t, testCaseRequestBackup{
		opts: requestBackupOpts{
			etcdBackupPolicyName: "test-backup-policy",
		},
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour), func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
				backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
					Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
						EtcdBackupStorageLocal: operatorv1alpha1.EtcdBackupStorageLocal{
							HostPath: "/etc/etcdbackups",
						},
					},
				}
				backupPolicy.Spec.Assets = []operatorv1alpha1.EtcdBackupAssetType{operatorv1alpha1.EtcdBackupAssetTypeSnapshot}
			})},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Verify new backup was created with assets from policy
			backups, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(context.TODO(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backups.Items, 1)

			backup := backups.Items[0]
			require.Equal(t, "test-backup-policy", backup.Labels[backuphelpers.LabelEtcdBackupPolicy])
			require.Equal(t, []operatorv1alpha1.EtcdBackupAssetType{operatorv1alpha1.EtcdBackupAssetTypeSnapshot}, backup.Spec.Assets)
		},
	})
}

// TestRequestBackupForPolicyAllNodesRetryAfterPartialCreate verifies that when a schedule execution in Local AllNodes
// mode fails after creating only some of its backups, retrying the same execution creates the missing backups instead
// of treating its own active backups as an overlapping execution.
func TestRequestBackupForPolicyAllNodesRetryAfterPartialCreate(t *testing.T) {
	const (
		policyName = "test-backup-policy"
		runName    = "test-backup-policy-29000000"
	)
	opts := requestBackupOpts{etcdBackupPolicyName: policyName, etcdBackupName: runName}
	createCount := 0

	runRequestBackupTest(t, testCaseRequestBackup{
		opts: opts,
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy(policyName, "@daily", testutils.WithBackupPolicyAge(25*time.Hour), func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
				backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
					Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
						EtcdBackupStorageLocal: operatorv1alpha1.EtcdBackupStorageLocal{
							HostPath: "/etc/etcdbackups",
						},
						Distribution: operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes,
					},
				}
			})},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-3", testutils.WithMasterLabel())},
		setup: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Fail the third create of the first execution attempt to simulate a transient API error.
			operatorFake.PrependReactor("create", "etcdbackups", func(action k8stesting.Action) (bool, runtime.Object, error) {
				createCount++
				if createCount == 3 {
					return true, nil, errors.New("transient create error")
				}
				return false, nil, nil
			})
		},
		// The first execution attempt fails after a partial create.
		expectError: true,
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			backupsClient := operatorFake.OperatorV1alpha1().EtcdBackups()
			backups, err := backupsClient.List(t.Context(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backups.Items, 2, "expected only a partial set of backups after the failed attempt")
			createActions := testutils.ListActions[k8stesting.CreateActionImpl](operatorFake.Actions())
			require.Len(t, createActions, 3)

			// Mark one of the existing backups as finished, it should not be recreated on retry.
			finishedBackup := &backups.Items[0]
			finishedBackup.Status.Conditions = append(finishedBackup.Status.Conditions, v1.Condition{
				Type:               string(operatorv1alpha1.BackupCompleted),
				Reason:             string(operatorv1alpha1.BackupReasonJobCompleted),
				Status:             v1.ConditionTrue,
				LastTransitionTime: v1.Now(),
			})
			_, err = backupsClient.Update(t.Context(), finishedBackup, v1.UpdateOptions{})
			require.NoError(t, err)

			// Retry the same execution: the missing backup must be created.
			operatorFake.ClearActions()
			err = opts.requestBackups(t.Context(), client, operatorFake.OperatorV1alpha1())
			require.NoError(t, err)

			createActions = testutils.ListActions[k8stesting.CreateActionImpl](operatorFake.Actions())
			require.Len(t, createActions, 1)
			backups, err = backupsClient.List(t.Context(), v1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, backups.Items, 3)

			backupNodes := make([]string, 0, len(backups.Items))
			for _, backup := range backups.Items {
				require.Equal(t, policyName, backup.Labels[backuphelpers.LabelEtcdBackupPolicy])
				require.Equal(t, runName, backup.Labels[backuphelpers.LabelEtcdBackupPolicyRun])
				backupNodes = append(backupNodes, backup.Spec.NodeSelector[corev1.LabelHostname])
			}
			require.ElementsMatch(t, []string{"test-node-1", "test-node-2", "test-node-3"}, backupNodes)
			require.Equal(t, 4, createCount, "expected the retry to create only the missing backup")
		},
	})
}

// TestRequestBackupForPolicyAllNodesWithActiveBackupFromOtherRun verifies that in Local AllNodes mode an active backup
// from a different schedule execution still causes the current execution to be skipped.
func TestRequestBackupForPolicyAllNodesWithActiveBackupFromOtherRun(t *testing.T) {
	runRequestBackupTest(t, testCaseRequestBackup{
		opts: requestBackupOpts{
			etcdBackupPolicyName: "test-backup-policy",
			etcdBackupName:       "test-backup-policy-29000060",
		},
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour), func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
				backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
					Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
						EtcdBackupStorageLocal: operatorv1alpha1.EtcdBackupStorageLocal{
							HostPath: "/etc/etcdbackups",
						},
						Distribution: operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes,
					},
				}
			})},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-policy-29000000-abc", testutils.WithBackupPolicy("test-backup-policy"), func(backup *operatorv1alpha1.EtcdBackup) {
				backup.Labels[backuphelpers.LabelEtcdBackupPolicyRun] = "test-backup-policy-29000000"
				backup.Spec.NodeSelector = map[string]string{corev1.LabelHostname: "test-node-1"}
			}),
		},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			_, ok := testutils.GetAction[k8stesting.CreateActionImpl](operatorFake.Fake.Actions())
			require.False(t, ok, "expected no create action")
		},
	})
}

// TestRequestBackupForPolicySingleNodeRoundRobin verifies that Local SingleNode backups rotate through the selected
// control plane nodes, ordered lexicographically by node name, based on the node targeted by the policy's most recent backup.
func TestRequestBackupForPolicySingleNodeRoundRobin(t *testing.T) {
	const policyName = "test-backup-policy"
	localStorage := operatorv1alpha1.EtcdBackupStorage{
		Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
		Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
	}
	// localBackup creates a Local backup pinned to hostname. statusNodeName is the node name recorded in status once
	// the backup job is scheduled, empty if it was never scheduled.
	localBackup := func(name, hostname, statusNodeName string, age time.Duration, status func(*operatorv1alpha1.EtcdBackup)) *operatorv1alpha1.EtcdBackup {
		return testutils.FakeEtcdBackup(name,
			testutils.WithBackupPolicy(policyName),
			testutils.WithBackupStorage(localStorage),
			testutils.WithBackupAge(age),
			func(backup *operatorv1alpha1.EtcdBackup) {
				backup.Spec.NodeSelector = map[string]string{corev1.LabelHostname: hostname}
			},
			status,
			// Override the default node name set by the status helpers
			func(backup *operatorv1alpha1.EtcdBackup) {
				backup.Status.NodeName = statusNodeName
			},
		)
	}
	completed := testutils.WithBackupCompleted()

	for _, tc := range []struct {
		name  string
		nodes []string
		// hostnames overrides the hostname label of a node, by node name. Defaults to the node name.
		hostnames    map[string]string
		backups      []*operatorv1alpha1.EtcdBackup
		expectedNode string
	}{
		{
			name:         "no previous backups selects first node",
			nodes:        []string{"test-node-3", "test-node-1", "test-node-2"},
			expectedNode: "test-node-1",
		},
		{
			name:         "selects node after most recent backup",
			nodes:        []string{"test-node-3", "test-node-1", "test-node-2"},
			backups:      []*operatorv1alpha1.EtcdBackup{localBackup("b1", "test-node-1", "test-node-1", time.Hour, completed)},
			expectedNode: "test-node-2",
		},
		{
			name:         "wraps around after last node",
			nodes:        []string{"test-node-3", "test-node-1", "test-node-2"},
			backups:      []*operatorv1alpha1.EtcdBackup{localBackup("b1", "test-node-3", "test-node-3", time.Hour, completed)},
			expectedNode: "test-node-1",
		},
		{
			name:  "uses most recently created backup",
			nodes: []string{"test-node-3", "test-node-1", "test-node-2"},
			backups: []*operatorv1alpha1.EtcdBackup{
				localBackup("b1", "test-node-1", "test-node-1", 3*time.Hour, completed),
				localBackup("b2", "test-node-2", "test-node-2", 1*time.Hour, completed),
				localBackup("b3", "test-node-3", "test-node-3", 2*time.Hour, completed),
			},
			expectedNode: "test-node-3",
		},
		{
			name:  "failed backup still advances rotation",
			nodes: []string{"test-node-3", "test-node-1", "test-node-2"},
			backups: []*operatorv1alpha1.EtcdBackup{
				localBackup("b1", "test-node-1", "test-node-1", 2*time.Hour, completed),
				localBackup("b2", "test-node-2", "test-node-2", 1*time.Hour, testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed)),
			},
			expectedNode: "test-node-3",
		},
		{
			name:         "previous node removed selects next node name",
			nodes:        []string{"test-node-3", "test-node-1"},
			backups:      []*operatorv1alpha1.EtcdBackup{localBackup("b1", "test-node-2", "test-node-2", time.Hour, completed)},
			expectedNode: "test-node-3",
		},
		{
			name:  "ignores backups not pinned to a node",
			nodes: []string{"test-node-3", "test-node-1", "test-node-2"},
			backups: []*operatorv1alpha1.EtcdBackup{
				localBackup("b1", "test-node-1", "test-node-1", 2*time.Hour, completed),
				testutils.FakeEtcdBackup("pvc", testutils.WithBackupPolicy(policyName), testutils.WithBackupAge(time.Hour), testutils.WithBackupCompleted()),
			},
			expectedNode: "test-node-2",
		},
		{
			// By hostname the order would be node-c, node-b, node-a, selecting node-c after host-3.
			name:         "orders by node name rather than hostname",
			nodes:        []string{"node-c", "node-a", "node-b"},
			hostnames:    map[string]string{"node-a": "host-3", "node-b": "host-2", "node-c": "host-1"},
			backups:      []*operatorv1alpha1.EtcdBackup{localBackup("b1", "host-3", "", time.Hour, completed)},
			expectedNode: "node-b",
		},
		{
			name:      "prefers node name from backup status",
			nodes:     []string{"node-c", "node-a", "node-b"},
			hostnames: map[string]string{"node-a": "host-3", "node-b": "host-2", "node-c": "host-1"},
			backups: []*operatorv1alpha1.EtcdBackup{
				localBackup("b1", "host-removed", "node-b", time.Hour, completed),
			},
			expectedNode: "node-c",
		},
		{
			name:         "previous node removed without status falls back to hostname",
			nodes:        []string{"node-c", "node-a"},
			backups:      []*operatorv1alpha1.EtcdBackup{localBackup("b1", "node-b", "", time.Hour, completed)},
			expectedNode: "node-c",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := make([]*corev1.Node, 0, len(tc.nodes))
			expectedHostname := tc.expectedNode
			for _, name := range tc.nodes {
				hostname, ok := tc.hostnames[name]
				if !ok {
					hostname = name
				}
				if name == tc.expectedNode {
					expectedHostname = hostname
				}
				nodes = append(nodes, testutils.FakeNode(name, testutils.WithMasterLabel(), func(node *corev1.Node) {
					node.Labels[corev1.LabelHostname] = hostname
				}))
			}
			runRequestBackupTest(t, testCaseRequestBackup{
				opts: requestBackupOpts{etcdBackupPolicyName: policyName, etcdBackupName: "test-backup-policy-29000000"},
				backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
					testutils.FakeEtcdBackupPolicy(policyName, "@daily", testutils.WithBackupPolicyAge(25*time.Hour), func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
						backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
							Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
							Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
								EtcdBackupStorageLocal: localStorage.Local,
								Distribution:           operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeSingleNode,
							},
						}
					})},
				backups: tc.backups,
				nodes:   nodes,
				validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
					createActions := testutils.ListActions[k8stesting.CreateActionImpl](operatorFake.Actions())
					require.Len(t, createActions, 1)
					backup, ok := createActions[0].Object.(*operatorv1alpha1.EtcdBackup)
					require.True(t, ok, "expected EtcdBackup to be created")
					require.Equal(t, expectedHostname, backup.Spec.NodeSelector[corev1.LabelHostname])
				},
			})
		})
	}
}

// TestRequestBackupForPolicySkipsExistingRun verifies that in Local SingleNode and PVC modes, a schedule execution
// does not create a new backup if any backup already exists for that run, regardless of whether it is still active,
// completed, or failed.
func TestRequestBackupForPolicySkipsExistingRun(t *testing.T) {
	const (
		policyName = "test-backup-policy"
		runName    = "test-backup-policy-29000000"
	)
	localStorage := operatorv1alpha1.EtcdBackupStorage{
		Type:  operatorv1alpha1.EtcdBackupStorageTypeLocal,
		Local: operatorv1alpha1.EtcdBackupStorageLocal{HostPath: "/etc/etcdbackups"},
	}
	pvcStorage := operatorv1alpha1.EtcdBackupStorage{
		Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
		PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "etcd-backup-pvc"},
	}

	for _, storage := range []struct {
		name       string
		policyOpt  func(*operatorv1alpha1.EtcdBackupPolicy)
		backupOpts []func(*operatorv1alpha1.EtcdBackup)
	}{
		{
			name: "local single node",
			policyOpt: func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
				backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
					Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
						EtcdBackupStorageLocal: localStorage.Local,
						Distribution:           operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeSingleNode,
					},
				}
			},
			backupOpts: []func(*operatorv1alpha1.EtcdBackup){
				testutils.WithBackupStorage(localStorage),
				func(backup *operatorv1alpha1.EtcdBackup) {
					backup.Spec.NodeSelector = map[string]string{corev1.LabelHostname: "test-node-1"}
				},
			},
		},
		{
			name: "pvc",
			policyOpt: func(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
				backupPolicy.Spec.Storage = operatorv1alpha1.EtcdBackupPolicyStorage{
					Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
					PVC:  operatorv1alpha1.EtcdBackupPolicyStoragePvc{EtcdBackupStoragePvc: pvcStorage.PVC},
				}
			},
			backupOpts: []func(*operatorv1alpha1.EtcdBackup){
				testutils.WithBackupStorage(pvcStorage),
			},
		},
	} {
		for _, state := range []struct {
			name   string
			status func(*operatorv1alpha1.EtcdBackup)
		}{
			{name: "active", status: func(*operatorv1alpha1.EtcdBackup) {}},
			{name: "completed", status: testutils.WithBackupCompleted()},
			{name: "failed", status: testutils.WithBackupFailed(operatorv1alpha1.BackupReasonJobFailed)},
		} {
			t.Run(storage.name+"/"+state.name, func(t *testing.T) {
				backupOpts := append([]func(*operatorv1alpha1.EtcdBackup){
					testutils.WithBackupPolicy(policyName),
					testutils.WithBackupAge(time.Minute),
					func(backup *operatorv1alpha1.EtcdBackup) {
						backup.Labels[backuphelpers.LabelEtcdBackupPolicyRun] = runName
					},
				}, storage.backupOpts...)
				backupOpts = append(backupOpts, state.status)

				runRequestBackupTest(t, testCaseRequestBackup{
					opts: requestBackupOpts{etcdBackupPolicyName: policyName, etcdBackupName: runName},
					backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
						testutils.FakeEtcdBackupPolicy(policyName, "@daily", testutils.WithBackupPolicyAge(25*time.Hour), storage.policyOpt)},
					backups: []*operatorv1alpha1.EtcdBackup{testutils.FakeEtcdBackup(runName, backupOpts...)},
					nodes: []*corev1.Node{
						testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
						testutils.FakeNode("test-node-2", testutils.WithMasterLabel())},
					validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
						_, ok := testutils.GetAction[k8stesting.CreateActionImpl](operatorFake.Fake.Actions())
						require.False(t, ok, "expected no create action")

						backups, err := operatorFake.OperatorV1alpha1().EtcdBackups().List(t.Context(), v1.ListOptions{})
						require.NoError(t, err)
						require.Len(t, backups.Items, 1)
						require.Equal(t, runName, backups.Items[0].Name)
					},
				})
			})
		}
	}
}

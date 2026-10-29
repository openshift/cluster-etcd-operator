package requestbackup

import (
	"context"
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
				// Local backup is assigned to a specific node, selected in lexicographical order
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
						NodeCount: 3,
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

func TestRequestBackupForPolicyWithExistingBackups(t *testing.T) {
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
						NodeCount: 2,
					},
				}
			})},
		backups: []*operatorv1alpha1.EtcdBackup{
			testutils.FakeEtcdBackup("test-backup-1", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupNodeName("test-node-1"), testutils.WithBackupCompleted()),
			testutils.FakeEtcdBackup("test-backup-2", testutils.WithBackupPolicy("test-backup-policy"), testutils.WithBackupNodeName("test-node-3"), testutils.WithBackupCompleted()),
			testutils.FakeEtcdBackup("test-backup-for-different-policy", testutils.WithBackupPolicy("test-different-backup-policy"), testutils.WithBackupNodeName("test-node-2"), testutils.WithBackupCompleted()),
		},
		nodes: []*corev1.Node{
			testutils.FakeNode("test-node-1", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-2", testutils.WithMasterLabel()),
			testutils.FakeNode("test-node-3", testutils.WithMasterLabel())},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Verify new backups were created with preference for nodes that already had backups
			createActions := testutils.ListActions[k8stesting.CreateActionImpl](operatorFake.Fake.Actions())
			require.Len(t, createActions, 2)

			expectedNodes := []string{"test-node-1", "test-node-3"}
			backupNodes := make([]string, 2)
			for i, action := range createActions {
				backup, ok := action.Object.(*operatorv1alpha1.EtcdBackup)
				require.True(t, ok, "expected etcdbackup creation")

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

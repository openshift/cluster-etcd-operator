package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	g "github.com/onsi/ginkgo/v2"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
)

const (
	OpenShiftEtcdNamespace = "openshift-etcd"
	HostPathBasePath       = "/etc/kubernetes/cluster-backup/"

	// ShellImage allows us to have basic shell tooling, taken from origin:
	// https://github.com/openshift/origin/blob/6ee9dc56a612a4c886d094571832ed47efa2e831/test/extended/util/image/image.go#L129-L141C2
	ShellImage = "image-registry.openshift-image-registry.svc:5000/openshift/tools:latest"
)

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestBackupHappyPath [Timeout:30m]", func() {
		TestBackupHappyPath(g.GinkgoTB())
	})
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestPeriodicBackupHappyPath [Timeout:30m]", func() {
		TestPeriodicBackupHappyPath(g.GinkgoTB())
	})
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestPeriodicBackupLocalAllNodes [Timeout:30m]", func() {
		TestPeriodicBackupLocalAllNodes(g.GinkgoTB())
	})
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestRetentionBySize [Timeout:30m]", func() {
		TestRetentionBySize(g.GinkgoTB())
	})
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestBackupPolicyRetentionAcrossPVCs [Timeout:30m]", func() {
		TestBackupPolicyRetentionAcrossPVCs(g.GinkgoTB())
	})
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestBackupFailureOnMissingPVC", func() {
		TestBackupFailureOnMissingPVC(g.GinkgoTB())
	})
	g.It("[Operator][Serial][Disruptive][OCPFeatureGate:AutomatedEtcdBackup] TestInvalidBackupPolicySchedule", func() {
		TestInvalidBackupPolicySchedule(g.GinkgoTB())
	})
})

func TestBackupHappyPath(t testing.TB) {
	pvcName := createHostPathPVC(t, "backup-happy-path-pvc")
	c := framework.NewOperatorClient(t).OperatorV1alpha1()

	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-happy-path",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: pvcName},
			},
		},
	}

	_, err := c.EtcdBackups().Create(context.Background(), &backup, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		deleteEtcdBackup(t, context.Background(), c, &backup)
		ensureEtcdBackupRemoved(t, context.Background(), backup.Name)
		ensureAllBackupPodsAreRemoved(t, context.Background())
	})

	pollContext, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(pollContext, 30*time.Second, false,
		func(ctx context.Context) (done bool, err error) {
			b, err := c.EtcdBackups().Get(ctx, backup.Name, metav1.GetOptions{})
			if err != nil {
				klog.Infof("error while getting backup: %v", err)
				return false, nil
			}

			klog.Infof("current backup job: %v", b.Status.JobName)
			klog.Infof("current backup conditions: %v", b.Status.Conditions)

			backupSuccess := backupHasCondition(b, operatorv1alpha1.BackupCompleted, metav1.ConditionTrue)
			if !backupSuccess {
				if backupHasCondition(b, operatorv1alpha1.BackupFailed, metav1.ConditionTrue) {
					return true, fmt.Errorf("unexpected backup failure found in backup conditions: %v", b.Status.Conditions)
				}
			}

			return backupSuccess, nil
		})
	require.NoError(t, err)

	foundFiles := collectFilesOnPVC(t, pvcName)
	expectedDirectory := path.Join(backup.Spec.Storage.PVC.Path, backup.Name)
	requireBackupFilesFound(t, expectedDirectory, foundFiles)
}

func TestPeriodicBackupHappyPath(t testing.TB) {
	pvcName := createHostPathPVC(t, "periodic-backup-happy-path-pvc")
	operatorClient := framework.NewOperatorClient(t)

	backupPolicy := operatorv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "periodic-backup-happy-path",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupPolicySpec{
			Schedule: "* * * * *",
			RetentionRules: []operatorv1alpha1.EtcdBackupPolicyRetentionRule{
				{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeQuantity,
					Quantity: operatorv1alpha1.EtcdBackupPolicyRetentionQuantity{
						MaxQuantity: 5,
					},
				},
			},
			Storage: operatorv1alpha1.EtcdBackupPolicyStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: operatorv1alpha1.EtcdBackupPolicyStoragePvc{
					EtcdBackupStoragePvc: operatorv1alpha1.EtcdBackupStoragePvc{Name: pvcName}},
			},
		},
	}

	_, err := operatorClient.OperatorV1alpha1().EtcdBackupPolicies().Create(context.Background(), &backupPolicy, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		stopBackupPolicyGracefully(t, context.Background(), backupPolicy.Name)
		deleteEtcdBackupPolicy(t, context.Background(), operatorClient.OperatorV1alpha1(), &backupPolicy)
		ensureEtcdBackupPolicyBackupsRemoved(t, context.Background(), backupPolicy.Name)
		ensureAllBackupPodsAreRemoved(t, context.Background())
	})

	retained, pruned := awaitBackupRetention(t, backupPolicy)
	files := collectFilesOnPVC(t, pvcName)
	requireBackupFilesMatchRetention(t, files, backupPolicy.Spec.Storage.PVC.Path, retained, pruned)
}

func TestPeriodicBackupLocalAllNodes(t testing.TB) {
	operatorClient := framework.NewOperatorClient(t)

	backupPolicy := operatorv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "periodic-backup-local-all-nodes",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupPolicySpec{
			Schedule: "* * * * *",
			RetentionRules: []operatorv1alpha1.EtcdBackupPolicyRetentionRule{
				{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeQuantity,
					Quantity: operatorv1alpha1.EtcdBackupPolicyRetentionQuantity{
						MaxQuantity: 2,
					},
				},
			},
			Storage: operatorv1alpha1.EtcdBackupPolicyStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypeLocal,
				Local: operatorv1alpha1.EtcdBackupPolicyStorageLocal{
					Distribution: operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes,
				},
			},
		},
	}
	_, err := operatorClient.OperatorV1alpha1().EtcdBackupPolicies().Create(context.Background(), &backupPolicy, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		stopBackupPolicyGracefully(t, context.Background(), backupPolicy.Name)
		deleteEtcdBackupPolicy(t, context.Background(), operatorClient.OperatorV1alpha1(), &backupPolicy)
		ensureEtcdBackupPolicyBackupsRemoved(t, context.Background(), backupPolicy.Name)
		ensureAllBackupPodsAreRemoved(t, context.Background())
	})

	retained, pruned := awaitBackupRetention(t, backupPolicy)
	coreClient := framework.NewCoreClient(t)
	controlPlaneNodeList, err := coreClient.Nodes().List(context.Background(), metav1.ListOptions{
		LabelSelector: backuphelpers.ControlPlaneNodeLabelSelector,
	})
	require.NoError(t, err)

	retainedByNode := map[string][]operatorv1alpha1.EtcdBackup{}
	prunedByNode := map[string][]operatorv1alpha1.EtcdBackup{}
	for _, backup := range retained {
		retainedByNode[backup.Status.NodeName] = append(retainedByNode[backup.Status.NodeName], backup)
	}
	for _, backup := range pruned {
		prunedByNode[backup.Status.NodeName] = append(prunedByNode[backup.Status.NodeName], backup)
	}

	for _, node := range controlPlaneNodeList.Items {
		retainedOnNode := retainedByNode[node.Name]
		prunedOnNode := prunedByNode[node.Name]
		require.Len(t, retainedOnNode, int(backupPolicy.Spec.RetentionRules[0].Quantity.MaxQuantity))
		require.Greater(t, len(prunedOnNode), 0)

		files := collectFilesOnNode(t, node.Name, backuphelpers.BackupPathMount)
		requireBackupFilesMatchRetention(t, files, "", retainedByNode[node.Name], prunedOnNode)
	}
}

func TestRetentionBySize(t testing.TB) {
	pvcName := createHostPathPVC(t, "retention-by-size")
	operatorClient := framework.NewOperatorClient(t)

	pushRandomConfigMaps(t)

	backupPolicy := operatorv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retention-by-size",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupPolicySpec{
			Schedule: "* * * * *",
			RetentionRules: []operatorv1alpha1.EtcdBackupPolicyRetentionRule{
				{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeSize,
					Size: operatorv1alpha1.EtcdBackupPolicyRetentionSize{
						MaxTotalSizeGiB: 1,
					},
				},
			},
			Storage: operatorv1alpha1.EtcdBackupPolicyStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: operatorv1alpha1.EtcdBackupPolicyStoragePvc{
					EtcdBackupStoragePvc: operatorv1alpha1.EtcdBackupStoragePvc{Name: pvcName}},
			},
		},
	}

	_, err := operatorClient.OperatorV1alpha1().EtcdBackupPolicies().Create(context.Background(), &backupPolicy, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		stopBackupPolicyGracefully(t, context.Background(), backupPolicy.Name)
		deleteEtcdBackupPolicy(t, context.Background(), operatorClient.OperatorV1alpha1(), &backupPolicy)
		ensureEtcdBackupPolicyBackupsRemoved(t, context.Background(), backupPolicy.Name)
		ensureAllBackupPodsAreRemoved(t, context.Background())
	})

	retained, pruned := awaitBackupRetention(t, backupPolicy)
	files := collectFilesOnPVC(t, pvcName)
	requireBackupFilesMatchRetention(t, files, backupPolicy.Spec.Storage.PVC.Path, retained, pruned)
}

func TestBackupPolicyRetentionAcrossPVCs(t testing.TB) {
	pvcName1 := createHostPathPVC(t, "backup-retention-across-pvcs-1")
	pvcName2 := createHostPathPVC(t, "backup-retention-across-pvcs-2")
	operatorClient := framework.NewOperatorClient(t)

	backupPolicy := &operatorv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-retention-across-pvcs",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupPolicySpec{
			Schedule: "* * * * *",
			RetentionRules: []operatorv1alpha1.EtcdBackupPolicyRetentionRule{
				{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeQuantity,
					Quantity: operatorv1alpha1.EtcdBackupPolicyRetentionQuantity{
						MaxQuantity: 2,
					},
				},
			},
			Storage: operatorv1alpha1.EtcdBackupPolicyStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: operatorv1alpha1.EtcdBackupPolicyStoragePvc{
					EtcdBackupStoragePvc: operatorv1alpha1.EtcdBackupStoragePvc{Name: pvcName1}},
			},
		},
	}

	backupPolicy, err := operatorClient.OperatorV1alpha1().EtcdBackupPolicies().Create(context.Background(), backupPolicy, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		stopBackupPolicyGracefully(t, context.Background(), backupPolicy.Name)
		deleteEtcdBackupPolicy(t, context.Background(), operatorClient.OperatorV1alpha1(), backupPolicy)
		ensureEtcdBackupPolicyBackupsRemoved(t, context.Background(), backupPolicy.Name)
		ensureAllBackupPodsAreRemoved(t, context.Background())
	})

	retained, pruned := awaitBackupRetention(t, *backupPolicy)
	files := collectFilesOnPVC(t, pvcName1)
	requireBackupFilesMatchRetention(t, files, backupPolicy.Spec.Storage.PVC.Path, retained, pruned)

	// Update backup policy to the new PVC
	patchBody, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"storage": map[string]any{
				"pvc": map[string]any{
					"name": pvcName2,
				},
			},
		},
	})
	require.NoError(t, err)
	backupPolicy, err = operatorClient.OperatorV1alpha1().EtcdBackupPolicies().Patch(context.Background(), backupPolicy.Name, types.MergePatchType, patchBody, metav1.PatchOptions{})
	require.NoError(t, err)
	setCronJobSuspend(t, context.Background(), backupPolicy.Name, false)

	// Wait for one of the old backups to be pruned
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	selector := backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicy.Name
	var backups []operatorv1alpha1.EtcdBackup
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, false, func(ctx context.Context) (bool, error) {
		backupList, err := operatorClient.OperatorV1alpha1().EtcdBackups().List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			klog.Infof("error while listing backups for policy %s: %v", backupPolicy.Name, err)
			return false, nil
		}

		backupsByName := make(map[string]operatorv1alpha1.EtcdBackup, len(backupList.Items))
		newBackupCompleted := false
		for _, backup := range backupList.Items {
			backupsByName[backup.Name] = backup
			if backup.Spec.Storage.PVC.Name == pvcName2 && backuphelpers.IsBackupCompleted(&backup) {
				newBackupCompleted = true
			}
		}
		if !newBackupCompleted {
			return false, nil
		}

		// Check if an old backup was pruned
		for _, backup := range retained {
			if _, ok := backupsByName[backup.Name]; !ok {
				backups = backupList.Items
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(backups), int(backupPolicy.Spec.RetentionRules[0].Quantity.MaxQuantity))
}

func TestBackupFailureOnMissingPVC(t testing.TB) {
	backup := operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failing-backup-missing-pvc",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			Storage: operatorv1alpha1.EtcdBackupStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC:  operatorv1alpha1.EtcdBackupStoragePvc{Name: "does-not-exist"},
			},
		},
	}

	c := framework.NewOperatorClient(t)
	_, err := c.OperatorV1alpha1().EtcdBackups().Create(context.Background(), &backup, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		err := c.OperatorV1alpha1().EtcdBackups().Delete(context.Background(), backup.Name, metav1.DeleteOptions{})
		require.NoError(t, err)
		ensureEtcdBackupRemoved(t, context.Background(), backup.Name)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			b, err := c.OperatorV1alpha1().EtcdBackups().Get(ctx, backup.Name, metav1.GetOptions{})
			if err != nil {
				klog.Infof("error while getting backup: %v", err)
				return false, nil
			}

			klog.Infof("current backup job: %v", b.Status.JobName)
			klog.Infof("current backup conditions: %v", b.Status.Conditions)

			condition := meta.FindStatusCondition(
				b.Status.Conditions,
				string(operatorv1alpha1.BackupFailed),
			)

			// Specific condition missing or it has not failed, let's keep waiting
			if condition == nil || condition.Status != metav1.ConditionTrue {
				return false, nil
			}

			// Condition exists and backup has failed, but the reason is wrong. Test fails
			if condition.Reason != string(operatorv1alpha1.BackupReasonSetupFailed) {
				return false, fmt.Errorf(
					"expected SetupFailed, got %q: %s",
					condition.Reason, condition.Message,
				)
			}

			// PVC is missing: no Job should have been created
			if b.Status.JobName != "" {
				return false, fmt.Errorf(
					"expected no backup Job, got %q", b.Status.JobName,
				)
			}

			return true, nil
		})
	require.NoError(t, err)
}

// pushRandomConfigMaps pushes about 125mb of random configmaps into the etcd cluster
func pushRandomConfigMaps(t testing.TB) {
	coreClient := framework.NewCoreClient(t)
	// we only allow retention on the order of gigabytes, so we have to fill up etcd for a while to get sizable snapshots
	// the maximum we can push without errors is: Too long: must have at most 1048576 bytes
	megString := rand.String(1024 * 1024 * 1)
	// pushing one hundred megabytes of CM takes about 2m with one thread, doing it 5x in parallel (kn)
	kn := 5
	n := 25

	wg := sync.WaitGroup{}
	wg.Add(kn)

	for k := range kn {
		go func(k int) {
			defer wg.Done()

			for i := range n {
				cm := corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      names.SimpleNameGenerator.GenerateName("retention-by-size"),
						Namespace: OpenShiftEtcdNamespace,
					},
					Data: map[string]string{
						"blob": megString,
					},
				}

				_, err := coreClient.ConfigMaps(OpenShiftEtcdNamespace).Create(context.Background(), &cm, metav1.CreateOptions{})
				if err != nil {
					klog.Errorf("[%d] got an error while creating filler configmap no. %d/%d: %v", k, i, n, err)
				}

				if i%5 == 0 {
					klog.Infof("[%d] writing CM %d/%d", k, i, n)
				}
			}
		}(k)
	}

	wg.Wait()
	t.Logf("done pushng %d configmaps at 1mb!", kn*n)
}

func TestInvalidBackupPolicySchedule(t testing.TB) {
	operatorClient := framework.NewOperatorClient(t)
	backupPolicies := operatorClient.OperatorV1alpha1().EtcdBackupPolicies()
	cronJobs := framework.NewBatchClient(t).CronJobs(OpenShiftEtcdNamespace)

	backupPolicy := operatorv1alpha1.EtcdBackupPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "periodic-backup-wrong-schedule",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupPolicySpec{
			Schedule: "*/5 */0 * * *",
			RetentionRules: []operatorv1alpha1.EtcdBackupPolicyRetentionRule{
				{
					Type: operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeQuantity,
					Quantity: operatorv1alpha1.EtcdBackupPolicyRetentionQuantity{
						MaxQuantity: 5,
					},
				},
			},
			Storage: operatorv1alpha1.EtcdBackupPolicyStorage{
				Type: operatorv1alpha1.EtcdBackupStorageTypePVC,
				PVC: operatorv1alpha1.EtcdBackupPolicyStoragePvc{
					EtcdBackupStoragePvc: operatorv1alpha1.EtcdBackupStoragePvc{Name: "not-existing-pvc"}},
			},
		},
	}

	createdPolicy, err := backupPolicies.Create(context.Background(), &backupPolicy, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		err := backupPolicies.Delete(context.Background(), backupPolicy.Name, metav1.DeleteOptions{})
		require.NoError(t, err)
		ensureCronJobRemoved(t, context.Background(), backupPolicy.Name)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			policy, err := backupPolicies.Get(ctx, backupPolicy.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			condition := meta.FindStatusCondition(policy.Status.Conditions, string(operatorv1alpha1.BackupPolicyScheduled))
			if condition == nil || condition.ObservedGeneration != createdPolicy.Generation {
				return false, nil
			}
			if condition.Status != metav1.ConditionFalse || condition.Reason != string(operatorv1alpha1.BackupPolicyInvalidSchedule) {
				return false, fmt.Errorf("expected Scheduled=False with reason InvalidSchedule, got %s with reason %s", condition.Status, condition.Reason)
			}

			cronJob, err := cronJobs.Get(ctx, backupPolicy.Name, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("unexpected CronJob %q for invalid schedule", cronJob.Name)
		})
	require.NoError(t, err)

	currentPolicy, err := backupPolicies.Get(context.Background(), backupPolicy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	currentPolicy.Spec.Schedule = "@daily"
	updatedPolicy, err := backupPolicies.Update(context.Background(), currentPolicy, metav1.UpdateOptions{})
	require.NoError(t, err)

	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer recoveryCancel()
	err = wait.PollUntilContextCancel(recoveryCtx, 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			policy, err := backupPolicies.Get(ctx, backupPolicy.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			condition := meta.FindStatusCondition(policy.Status.Conditions, string(operatorv1alpha1.BackupPolicyScheduled))
			return condition != nil &&
				condition.ObservedGeneration == updatedPolicy.Generation &&
				condition.Status == metav1.ConditionTrue &&
				condition.Reason == string(operatorv1alpha1.BackupPolicySchedulingSucceeded), nil
		})
	require.NoError(t, err, "expected Scheduled=True with reason SchedulingSucceeded after correcting the schedule")

	cronJob, err := cronJobs.Get(context.Background(), backupPolicy.Name, metav1.GetOptions{})
	require.NoError(t, err, "expected CronJob after correcting the schedule")
	require.Equal(t, "@daily", cronJob.Spec.Schedule)
}

func awaitBackupRetention(t testing.TB, backupPolicy operatorv1alpha1.EtcdBackupPolicy) (retained, pruned []operatorv1alpha1.EtcdBackup) {
	t.Helper()
	require.Len(t, backupPolicy.Spec.RetentionRules, 1, "expected a single retention rule")

	backups := framework.NewOperatorClient(t).OperatorV1alpha1().EtcdBackups()
	selector := backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicy.Name
	// Remember completed backups across polls: pruning removes them from later lists.
	completedSeen := map[string]operatorv1alpha1.EtcdBackup{}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 5*time.Second, false, func(ctx context.Context) (bool, error) {
		list, err := backups.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			klog.Infof("error while listing backups for policy %s: %v", backupPolicy.Name, err)
			return false, nil
		}

		var satisfied bool
		retained, pruned, satisfied, err = checkBackupRetention(backupPolicy, list.Items, completedSeen)
		return satisfied && len(pruned) > 0, err
	})
	require.NoErrorf(t, err, "waiting for %s retention: saw %d completed backups and %d pruned backups", backupPolicy.Spec.RetentionRules[0].Type, len(completedSeen), len(pruned))
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer settleCancel()
	// Stop new requests while keeping the policy active so retention can finish.
	cronJob := setCronJobSuspend(t, settleCtx, backupPolicy.Name, true)
	batchClient := framework.NewBatchClient(t)
	err = wait.PollUntilContextCancel(settleCtx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		requests, err := batchClient.Jobs(OpenShiftEtcdNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=cluster-backup-cronjob"})
		if err != nil {
			return false, err
		}
		for _, job := range requests.Items {
			if metav1.IsControlledBy(&job, cronJob) && job.Status.CompletionTime == nil {
				return false, nil
			}
		}
		list, err := backups.List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, err
		}
		var satisfied bool
		retained, pruned, satisfied, err = checkBackupRetention(backupPolicy, list.Items, completedSeen)
		// All remaining backups must be completed and none may still be deleting.
		return satisfied && len(retained) == len(list.Items), err
	})
	require.NoError(t, err, "waiting for backup requests, backups and retention to finish")
	require.NotEmpty(t, retained, "retention must keep at least the newest completed backup")
	return retained, pruned
}

func checkBackupRetention(backupPolicy operatorv1alpha1.EtcdBackupPolicy, backups []operatorv1alpha1.EtcdBackup, completedSeen map[string]operatorv1alpha1.EtcdBackup) (retained, pruned []operatorv1alpha1.EtcdBackup, satisfied bool, err error) {
	current := make(map[string]struct{}, len(backups))
	retainedSize := map[string]int64{}
	retainedCount := map[string]int32{}
	isGrouped := backupPolicy.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypeLocal && backupPolicy.Spec.Storage.Local.Distribution == operatorv1alpha1.EtcdBackupPolicyLocalDistributionTypeAllNodes
	for _, backup := range backups {
		current[backup.Name] = struct{}{}
		if backupHasCondition(&backup, operatorv1alpha1.BackupFailed, metav1.ConditionTrue) {
			return nil, nil, false, fmt.Errorf("backup %s failed: %v", backup.Name, backup.Status.Conditions)
		}
		completed := backupHasCondition(&backup, operatorv1alpha1.BackupCompleted, metav1.ConditionTrue)
		if completed {
			completedSeen[backup.Name] = backup
		}
		if backup.DeletionTimestamp != nil || !completed {
			continue
		}
		retained = append(retained, backup)

		key := ""
		if isGrouped {
			key = backup.Status.NodeName
		}
		retainedCount[key] += 1
		for _, file := range backup.Status.Files {
			retainedSize[key] += file.SizeBytes
		}
	}
	for name, backup := range completedSeen {
		if _, present := current[name]; !present {
			pruned = append(pruned, backup)
		}
	}

	satisfied = true
	rule := backupPolicy.Spec.RetentionRules[0]
	switch rule.Type {
	case operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeQuantity:
		for _, count := range retainedCount {
			if count != rule.Quantity.MaxQuantity {
				satisfied = false
			}
		}
	case operatorv1alpha1.EtcdBackupPolicyRetentionRuleTypeSize:
		maxBytes := int64(rule.Size.MaxTotalSizeGiB) * 1024 * 1024 * 1024
		for key, size := range retainedSize {
			// The newest backup is kept even when it exceeds the size limit on its own.
			count := retainedCount[key]
			// TODO: Fail if newest pruned backup would fit in maxBytes
			if count > 1 && size > maxBytes {
				satisfied = false
			}
		}
	default:
		return nil, nil, false, fmt.Errorf("unsupported retention rule %q", rule.Type)
	}
	return retained, pruned, satisfied, nil
}

func requireBackupFilesMatchRetention(t testing.TB, files []string, storagePath string, retained, pruned []operatorv1alpha1.EtcdBackup) {
	t.Helper()
	for _, backup := range retained {
		requireBackupFilesFound(t, path.Join(storagePath, backup.Name), files)
	}
	for _, backup := range pruned {
		prefix := "./" + path.Join(storagePath, backup.Name) + "/"
		for _, file := range files {
			require.Falsef(t, strings.HasPrefix(file, prefix), "files from pruned backup %s remain: %s", backup.Name, file)
		}
	}
}

func stopBackupPolicyGracefully(t testing.TB, ctx context.Context, policyName string) {
	t.Helper()
	policies := framework.NewOperatorClient(t).OperatorV1alpha1().EtcdBackupPolicies()
	cronJobs := framework.NewBatchClient(t).CronJobs(OpenShiftEtcdNamespace)
	// Keep the CronJob until we can delete it in the foreground, waiting for
	// its request Jobs and Pods to disappear before deleting backups.
	orphan := metav1.DeletePropagationOrphan
	err := policies.Delete(ctx, policyName, metav1.DeleteOptions{PropagationPolicy: &orphan})
	if !errors.IsNotFound(err) {
		require.NoError(t, err)
	}
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(pollCtx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := policies.Get(ctx, policyName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	require.NoError(t, err, "waiting for backup policy deletion")

	foreground := metav1.DeletePropagationForeground
	err = cronJobs.Delete(ctx, policyName, metav1.DeleteOptions{PropagationPolicy: &foreground})
	if !errors.IsNotFound(err) {
		require.NoError(t, err)
	}
	ensureCronJobRemoved(t, ctx, policyName)
}

func createHostPathPVC(t testing.TB, prefix string) string {
	t.Helper()
	pvcName := names.SimpleNameGenerator.GenerateName(prefix + "-")
	c := framework.NewCoreClient(t)
	// Registered before setup so it runs after the test's backup cleanup.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		pods, err := c.Pods(OpenShiftEtcdNamespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		for _, pod := range pods.Items {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvcName {
					if err := c.Pods(OpenShiftEtcdNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); !errors.IsNotFound(err) {
						require.NoError(t, err, "deleting test volume Pod")
					}
					break
				}
			}
		}
		if err := c.PersistentVolumeClaims(OpenShiftEtcdNamespace).Delete(ctx, pvcName, metav1.DeleteOptions{}); !errors.IsNotFound(err) {
			require.NoError(t, err, "deleting test PVC")
		}
		if err := c.PersistentVolumes().Delete(ctx, pvcName+"-pv", metav1.DeleteOptions{}); !errors.IsNotFound(err) {
			require.NoError(t, err, "deleting test PV")
		}
		err = wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			if _, err := c.PersistentVolumeClaims(OpenShiftEtcdNamespace).Get(ctx, pvcName, metav1.GetOptions{}); !errors.IsNotFound(err) {
				return false, err
			}
			_, err := c.PersistentVolumes().Get(ctx, pvcName+"-pv", metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
		require.NoError(t, err, "waiting for test PVC and PV to be removed")
	})
	ensureHostPathPVC(t, pvcName)
	return pvcName
}

func ensureHostPathPVC(t testing.TB, pvcName string) {
	t.Helper()
	kubeConfig, err := framework.NewClientConfigForTest("")
	require.NoError(t, err)

	c, err := corev1client.NewForConfig(kubeConfig)
	require.NoError(t, err)
	nodes, err := c.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: backuphelpers.ControlPlaneNodeLabelSelector})
	require.NoError(t, err)
	require.NotEmpty(t, nodes.Items, "a control-plane node is required for the backup volume")

	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName + "-pv",
		},
		Spec: corev1.PersistentVolumeSpec{
			// Backup, garbage collection and file inspection must use the same
			// node because hostPath directories are not shared between nodes.
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{nodes.Items[0].Name},
					}},
				}}},
			},
			StorageClassName: "manual",
			Capacity: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceStorage: *resource.NewQuantity(10, "Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: HostPathBasePath + pvcName,
				},
			},
		},
	}

	existingPV, err := c.PersistentVolumes().Get(context.Background(), pv.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_, err = c.PersistentVolumes().Create(context.Background(), &pv, metav1.CreateOptions{})
			require.NoErrorf(t, err, "could not create PV")
		} else {
			require.NoErrorf(t, err, "could not retrieve PV")
		}
	} else {
		require.Equal(t, pv.Spec.NodeAffinity, existingPV.Spec.NodeAffinity, "existing test PV %s has different topology; recreate the test PVC/PV before rerunning", pv.Name)
	}

	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:       pv.Name,
			StorageClassName: pointer.String("manual"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					corev1.ResourceStorage: *resource.NewQuantity(10, "Gi"),
				},
			},
		},
	}

	_, err = c.PersistentVolumeClaims(pvc.Namespace).Get(context.Background(), pvc.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_, err = c.PersistentVolumeClaims(pvc.Namespace).Create(context.Background(), &pvc, metav1.CreateOptions{})
			require.NoErrorf(t, err, "could not create PVC")
		} else {
			require.NoErrorf(t, err, "could not retrieve PVC")
		}
	}
}

func deleteEtcdBackup(t testing.TB, ctx context.Context, c v1alpha1.OperatorV1alpha1Interface, backup *operatorv1alpha1.EtcdBackup) {
	t.Helper()
	err := c.EtcdBackups().Delete(ctx, backup.Name, metav1.DeleteOptions{})
	if !errors.IsNotFound(err) {
		require.NoError(t, err)
	}
}

func deleteEtcdBackupPolicy(t testing.TB, ctx context.Context, c v1alpha1.OperatorV1alpha1Interface, backupPolicy *operatorv1alpha1.EtcdBackupPolicy) {
	t.Helper()
	err := c.EtcdBackupPolicies().Delete(ctx, backupPolicy.Name, metav1.DeleteOptions{})
	if !errors.IsNotFound(err) {
		require.NoError(t, err)
	}

	backupList, err := c.EtcdBackups().List(ctx, metav1.ListOptions{
		LabelSelector: backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicy.Name,
	})
	require.NoError(t, err)
	for _, backup := range backupList.Items {
		deleteEtcdBackup(t, ctx, c, &backup)
	}
}

func ensureCronJobRemoved(t testing.TB, ctx context.Context, name string) {
	t.Helper()
	c := framework.NewBatchClient(t)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		_, err := c.CronJobs(OpenShiftEtcdNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}

			klog.Infof("error while getting cronjob, waiting for its deletion: %v", err)
		}

		return false, nil
	})
	require.NoErrorf(t, err, "waiting for cronjob deletion error")
}

func ensureEtcdBackupRemoved(t testing.TB, ctx context.Context, backupName string) {
	t.Helper()
	c := framework.NewOperatorClient(t)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		_, err := c.OperatorV1alpha1().EtcdBackups().Get(ctx, backupName, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			klog.Infof("error while getting backups, waiting for deletion: %v", err)
		}
		return false, nil
	})
	require.NoErrorf(t, err, "waiting for etcdbackups deletion error")
}

func ensureEtcdBackupPolicyBackupsRemoved(t testing.TB, ctx context.Context, backupPolicyName string) {
	t.Helper()
	c := framework.NewOperatorClient(t)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		backupsList, err := c.OperatorV1alpha1().EtcdBackups().List(ctx, metav1.ListOptions{
			LabelSelector: backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicyName,
		})
		if err != nil {
			klog.Infof("error while getting backups, waiting for deletion: %v", err)
			return false, nil
		}

		if len(backupsList.Items) > 0 {
			klog.Infof("ensureEtcdBackupPolicyBackupsRemoved found %d backups for policy: %s", len(backupsList.Items), backupPolicyName)
			return false, nil
		}

		return true, nil
	})
	require.NoErrorf(t, err, "waiting for etcdbackups deletion error")
}

func ensureAllBackupPodsAreRemoved(t testing.TB, ctx context.Context) {
	t.Helper()
	c := framework.NewCoreClient(t)

	// Force-delete any lingering backup pods first
	podList, err := c.Pods(OpenShiftEtcdNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, p := range podList.Items {
			if strings.Contains(p.Name, "cluster-backup-job") {
				klog.Infof("Force-deleting backup pod: %s", p.Name)
				gracePeriod := int64(0)
				_ = c.Pods(OpenShiftEtcdNamespace).Delete(ctx, p.Name, metav1.DeleteOptions{
					GracePeriodSeconds: &gracePeriod,
				})
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		podList, err := c.Pods(OpenShiftEtcdNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.Infof("error while getting pods, waiting for its deletion: %v", err)
			return false, nil
		}

		for _, p := range podList.Items {
			if strings.Contains(p.Name, "cluster-backup-job") {
				klog.Infof("EnsureBackupPodRemoval found pod with name: %s", p.Name)
				return false, nil
			}
		}

		return true, nil
	})
	require.NoErrorf(t, err, "waiting for backup pods deletion error")
}

func backupHasCondition(backup *operatorv1alpha1.EtcdBackup,
	conditionType operatorv1alpha1.BackupConditionType,
	status metav1.ConditionStatus) bool {

	for _, condition := range backup.Status.Conditions {
		if string(conditionType) != condition.Type {
			continue
		}
		return condition.Status == status
	}
	return false
}

func collectFilesOnPVC(t testing.TB, pvcName string) []string {
	t.Helper()
	pod := creatFileFinderPod(t, func(p *corev1.Pod) {
		p.Spec.NodeSelector = map[string]string{
			backuphelpers.ControlPlaneNodeLabelSelector: "",
		}
		p.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		}
	})
	files := waitForPodLogLines(t, pod.Name)
	klog.Infof("found files in PVC [%s]: %v", pvcName, files)
	return files
}

func collectFilesOnNode(t testing.TB, nodeName string, hostPath string) []string {
	t.Helper()
	pod := creatFileFinderPod(t, func(p *corev1.Pod) {
		p.Spec.NodeName = nodeName
		p.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: hostPath,
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		}
	})
	files := waitForPodLogLines(t, pod.Name)
	klog.Infof("found files on Node [%s]: %v", nodeName, files)
	return files
}

func creatFileFinderPod(t testing.TB, opts ...func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	client := framework.NewCoreClient(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName("backup-finder-pod" + "-"),
			Namespace: OpenShiftEtcdNamespace,
			Labels: map[string]string{
				"app": "backup-finder",
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "backup-dir",
				},
			},
			Containers: []corev1.Container{
				{
					Name:                     "finder",
					Image:                    ShellImage,
					Command:                  []string{"find", "."},
					WorkingDir:               HostPathBasePath,
					Resources:                corev1.ResourceRequirements{},
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "backup-dir",
							ReadOnly:  false,
							MountPath: HostPathBasePath,
						},
					},
					SecurityContext: &corev1.SecurityContext{Privileged: pointer.Bool(true)},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   []corev1.Toleration{{Operator: "Exists"}},
		},
	}
	for _, opt := range opts {
		opt(pod)
	}
	_, err := client.Pods(OpenShiftEtcdNamespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		err := client.Pods(OpenShiftEtcdNamespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
		require.NoError(t, err)
	})
	return pod
}

func waitForPodLogLines(t testing.TB, name string) []string {
	t.Helper()
	client := framework.NewCoreClient(t)

	pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(pollCtx, 10*time.Second, false, func(ctx context.Context) (done bool, err error) {
		p, err := client.Pods(OpenShiftEtcdNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.Infof("error while getting pod: %v", err)
			return false, nil
		}

		if p.Status.Phase == corev1.PodFailed {
			return true, fmt.Errorf("pod failed with status: %s", p.Status.String())
		}

		return p.Status.Phase == corev1.PodSucceeded, nil
	})
	require.NoErrorf(t, err, "waiting for pod error")

	logBytes, err := client.Pods(OpenShiftEtcdNamespace).GetLogs(name, &corev1.PodLogOptions{}).Do(context.Background()).Raw()
	require.NoError(t, err)
	return strings.Split(string(logBytes), "\n")
}

func requireBackupFilesFound(t testing.TB, expectedDirectory string, files []string) {
	// expectedDirectory is relative to the inspected storage root, without a leading "./".
	// Example files:
	// ./backup-happy-path/static_kuberesources_2023-08-03_152316.tar.gz
	// ./backup-happy-path/snapshot_2023-08-03_152316.db

	// we assert that there are always at least two files:
	tarMatchFound := false
	snapMatchFound := false
	for _, file := range files {
		matchesTar, err := regexp.MatchString(`^\./`+regexp.QuoteMeta(expectedDirectory)+`/static_kuberesources_.*\.tar\.gz$`, file)
		require.NoError(t, err)
		if matchesTar {
			klog.Infof("Found matching kube resources: %s", file)
			tarMatchFound = true
		}

		matchesSnap, err := regexp.MatchString(`^\./`+regexp.QuoteMeta(expectedDirectory)+`/snapshot_.*\.db$`, file)
		require.NoError(t, err)
		if matchesSnap {
			klog.Infof("Found matching snapshot: %s", file)
			snapMatchFound = true
		}
	}

	require.Truef(t, tarMatchFound, "expected tarfile for backup: %s, found files: %v ", expectedDirectory, files)
	require.Truef(t, snapMatchFound, "expected snapshot for backup: %s, found files: %v ", expectedDirectory, files)
}

func setCronJobSuspend(t testing.TB, ctx context.Context, name string, suspend bool) *batchv1.CronJob {
	batchClient := framework.NewBatchClient(t)
	patchBody, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"suspend": suspend,
		},
	})
	require.NoError(t, err)
	cronJob, err := batchClient.CronJobs(OpenShiftEtcdNamespace).Patch(ctx, name, types.MergePatchType, patchBody, metav1.PatchOptions{})
	require.NoError(t, err, "suspending backup requests")
	return cronJob
}

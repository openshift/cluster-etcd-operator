package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	configv1alpha1 "github.com/openshift/api/config/v1alpha1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

const (
	CronJobKindName        = "CronJob"
	OpenShiftEtcdNamespace = "openshift-etcd"
	HostPathBasePath       = "/etc/kubernetes/cluster-backup/"

	// ShellImage allows us to have basic shell tooling, taken from origin:
	// https://github.com/openshift/origin/blob/6ee9dc56a612a4c886d094571832ed47efa2e831/test/extended/util/image/image.go#L129-L141C2
	ShellImage = "image-registry.openshift-image-registry.svc:5000/openshift/tools:latest"
)

func TestBackupHappyPath(t *testing.T) {
	pvcName := "backup-happy-path-pvc"
	ensureHostPathPVC(t, pvcName)
	c := framework.NewOperatorClient(t)

	backupCrd := operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-happy-path",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			PVCName: pvcName,
		},
	}

	_, err := c.OperatorV1alpha1().EtcdBackups().Create(context.Background(), &backupCrd, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		err := c.OperatorV1alpha1().EtcdBackups().Delete(context.Background(), backupCrd.Name, metav1.DeleteOptions{})
		require.NoError(t, err)

		ensureEtcdBackupsRemoved(t, backupCrd.Name)
		ensureAllBackupPodsAreRemoved(t)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 30*time.Second, false,
		func(ctx context.Context) (done bool, err error) {
			b, err := c.OperatorV1alpha1().EtcdBackups().Get(ctx, backupCrd.Name, metav1.GetOptions{})
			if err != nil {
				klog.Infof("error while getting backup: %v", err)
				return false, nil
			}

			klog.Infof("current backup job: %v", b.Status.BackupJob)
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

	foundFiles := collectFilesInPVCAcrossAllNodes(t, pvcName)
	requireBackupFilesFound(t, backupCrd.Name, foundFiles)
}

func TestPeriodicBackupHappyPath(t *testing.T) {
	pvcName := "periodic-backup-happy-path-pvc"
	ensureHostPathPVC(t, pvcName)
	configClient := framework.NewConfigClient(t)

	backupCrd := configv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "periodic-backup-happy-path",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: configv1alpha1.BackupSpec{
			EtcdBackupSpec: configv1alpha1.EtcdBackupSpec{
				Schedule: "* * * * *",
				TimeZone: "UTC",
				RetentionPolicy: configv1alpha1.RetentionPolicy{
					RetentionType:   configv1alpha1.RetentionTypeNumber,
					RetentionNumber: &configv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5},
				},
				PVCName: pvcName,
			},
		},
	}

	_, err := configClient.Backups().Create(context.Background(), &backupCrd, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		err := configClient.Backups().Delete(context.Background(), backupCrd.Name, metav1.DeleteOptions{})
		require.NoError(t, err)

		// deleting the CRD should delete all its cronjobs/jobs, etcdbackups and their pods as well
		ensureCronJobRemoved(t, backupCrd.Name)
		ensureEtcdBackupsRemoved(t, backupCrd.Name)
		ensureAllBackupPodsAreRemoved(t)
	})

	awaitBackupInvocations(t, backupCrd)

	foundFiles := collectFilesInPVCAcrossAllNodes(t, pvcName)
	grouped := groupBackupFilesByRunPrefix(t, foundFiles)
	// we expect either 5 or 6 backups, depending on whether there's currently an invocation ongoing
	require.Truef(t, len(grouped) == 5 || len(grouped) == 6,
		"expected 5 or 6 backup groups, but found %d. Groups: %v", len(grouped), grouped)
	// each group individually must be a valid backup
	// TODO(thomas): this might flake when an ongoing backup is not entirely done yet?
	for k, v := range grouped {
		t.Logf("testing backup group %s = %v", k, v)
		requireBackupFilesFound(t, backupCrd.Name, v)
	}
}

func TestRetentionBySize(t *testing.T) {
	pvcName := "retention-by-size"
	ensureHostPathPVC(t, pvcName)
	configClient := framework.NewConfigClient(t)

	pushRandomConfigMaps(t)

	backupCrd := configv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "retention-by-size",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: configv1alpha1.BackupSpec{
			EtcdBackupSpec: configv1alpha1.EtcdBackupSpec{
				Schedule: "* * * * *",
				TimeZone: "UTC",
				RetentionPolicy: configv1alpha1.RetentionPolicy{
					RetentionType: configv1alpha1.RetentionTypeSize,
					RetentionSize: &configv1alpha1.RetentionSizeConfig{MaxSizeOfBackupsGb: 1},
				},
				PVCName: pvcName,
			},
		},
	}

	_, err := configClient.Backups().Create(context.Background(), &backupCrd, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		err := configClient.Backups().Delete(context.Background(), backupCrd.Name, metav1.DeleteOptions{})
		require.NoError(t, err)
	})

	awaitBackupInvocations(t, backupCrd)
	// this is the state after five job invocations
	foundFiles := collectFilesInPVCAcrossAllNodes(t, pvcName)
	grouped := groupBackupFilesByRunPrefix(t, foundFiles)
	// we expect either 4 or 5 backups, depending on whether there's currently an invocation ongoing
	require.Truef(t, len(grouped) == 4 || len(grouped) == 5,
		"expected 4 or 5 backup groups, but found %d. Groups: %v", len(grouped), grouped)
	// each group individually must be a valid backup
	// TODO(thomas): this might flake when an ongoing backup is not entirely done yet?
	for k, v := range grouped {
		t.Logf("testing backup group %s = %v", k, v)
		requireBackupFilesFound(t, backupCrd.Name, v)
	}
}

func TestMultipleBackupsAreSkipped(t *testing.T) {
	pvcName := "multi-backups"
	ensureHostPathPVC(t, pvcName)
	c := framework.NewOperatorClient(t)

	backupCrd := operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			PVCName: pvcName,
		},
	}

	for i := 0; i < 5; i++ {
		backupCrd.Name = fmt.Sprintf("multi-backup-%d", i)
		_, err := c.OperatorV1alpha1().EtcdBackups().Create(context.Background(), &backupCrd, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// this is highly reliant on the sync interval of the backup controller, we expect at least one of the five to have condition "skipped"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 5*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			list, err := c.OperatorV1alpha1().EtcdBackups().List(context.Background(), metav1.ListOptions{})
			if err != nil {
				klog.Infof("error while listing backup: %v", err)
				return false, nil
			}

			for _, b := range list.Items {
				if backupHasCondition(&b, operatorv1alpha1.BackupSkipped, metav1.ConditionTrue) {
					return true, nil
				}
			}

			return false, nil
		})
	require.NoError(t, err)
}

func TestBackupFailureOnMissingPVC(t *testing.T) {
	backupCrd := operatorv1alpha1.EtcdBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failing-backup-missing-pvc",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: operatorv1alpha1.EtcdBackupSpec{
			PVCName: "something that does not exist",
		},
	}

	c := framework.NewOperatorClient(t)
	_, err := c.OperatorV1alpha1().EtcdBackups().Create(context.Background(), &backupCrd, metav1.CreateOptions{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 5*time.Second, true,
		func(ctx context.Context) (done bool, err error) {
			b, err := c.OperatorV1alpha1().EtcdBackups().Get(ctx, backupCrd.Name, metav1.GetOptions{})
			if err != nil {
				klog.Infof("error while getting backup: %v", err)
				return false, nil
			}

			klog.Infof("current backup job: %v", b.Status.BackupJob)
			klog.Infof("current backup conditions: %v", b.Status.Conditions)

			return backupHasCondition(b, operatorv1alpha1.BackupFailed, metav1.ConditionTrue), nil
		})
	require.NoError(t, err)
}

// pushRandomConfigMaps pushes about 125mb of random configmaps into the etcd cluster
func pushRandomConfigMaps(t *testing.T) {
	coreClient := framework.NewCoreClient(t)
	// we only allow retention on the order of gigabytes, so we have to fill up etcd for a while to get sizable snapshots
	// the maximum we can push without errors is: Too long: must have at most 1048576 bytes
	megString := rand.String(1024 * 1024 * 1)
	// pushing one hundred megabytes of CM takes about 2m with one thread, doing it 5x in parallel (kn)
	kn := 5
	n := 25

	wg := sync.WaitGroup{}
	wg.Add(kn)

	for k := 0; k < kn; k++ {
		go func(k int) {
			defer wg.Done()

			for i := 0; i < n; i++ {
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

func TestWrongScheduleDegradesOperator(t *testing.T) {
	operatorClient := framework.NewOperatorClient(t)
	configClient := framework.NewConfigClient(t)

	backupCrd := configv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "periodic-backup-wrong-schedule",
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: configv1alpha1.BackupSpec{
			EtcdBackupSpec: configv1alpha1.EtcdBackupSpec{
				Schedule: "*/5 */0 * * *",
				TimeZone: "UTC",
				RetentionPolicy: configv1alpha1.RetentionPolicy{
					RetentionType:   configv1alpha1.RetentionTypeNumber,
					RetentionNumber: &configv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5},
				},
				PVCName: "not-existing-pvc",
			},
		},
	}

	_, err := configClient.Backups().Create(context.Background(), &backupCrd, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = configClient.Backups().Delete(context.Background(), backupCrd.Name, metav1.DeleteOptions{})

	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 10*time.Second, false,
		func(ctx context.Context) (done bool, err error) {
			operatorStatus, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
			if err != nil {
				klog.Infof("error while getting operator status: %v", err)
				return false, nil
			}

			klog.Infof("current conditions: %v", operatorStatus.Status.Conditions)

			for _, condition := range operatorStatus.Status.Conditions {
				if condition.Type == "PeriodicBackupControllerDegraded" &&
					condition.Reason == "Error" &&
					string(condition.Status) == string(metav1.ConditionTrue) &&
					strings.Contains(condition.Message,
						"CronJob.batch \"periodic-backup-wrong-schedule\" is invalid: spec.schedule:") {
					return true, nil
				}
			}

			return false, nil
		})
	require.NoError(t, err, "expected operator to degrade")

	// deleting the offending backup should recover the operator condition again
	err = configClient.Backups().Delete(context.Background(), backupCrd.Name, metav1.DeleteOptions{})
	require.NoError(t, err)

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 10*time.Second, false,
		func(ctx context.Context) (done bool, err error) {
			operatorStatus, err := operatorClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
			if err != nil {
				klog.Infof("error while getting operator status: %v", err)
				return false, nil
			}

			klog.Infof("current conditions: %v", operatorStatus.Status.Conditions)

			for _, condition := range operatorStatus.Status.Conditions {
				if condition.Type == "PeriodicBackupControllerDegraded" &&
					condition.Reason == "AsExpected" &&
					string(condition.Status) == string(metav1.ConditionFalse) {
					return true, nil
				}
			}

			return false, nil
		})
	require.NoError(t, err, "expected operator to recover after degrade")
}

func awaitBackupInvocations(t *testing.T, backupCrd configv1alpha1.Backup) {
	batchClient := framework.NewBatchClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 30*time.Second, false,
		func(ctx context.Context) (done bool, err error) {
			jobList, err := batchClient.Jobs(backupCrd.Namespace).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				klog.Infof("error while getting job list: %v", err)
				return false, nil
			}

			// the cronjob will keep at most 5 successful runs, we're waiting until the list has that many jobs completed
			const required = 5
			succeededJobs := 0
			for _, j := range jobList.Items {
				ownedByOurCron := false
				for _, reference := range j.OwnerReferences {
					if reference.Kind == CronJobKindName && reference.Name == backupCrd.Name {
						ownedByOurCron = true
						break
					}
				}

				if ownedByOurCron {
					klog.Infof("Found job invocation: %s with status %s", j.Name, j.Status.String())
					if j.Status.Succeeded > 0 {
						succeededJobs++
					}
				}
			}

			if succeededJobs >= required {
				klog.Infof("found enough succeeded jobs")
				return true, nil
			}

			klog.Infof("found %d/%d succeeded jobs, retrying...", succeededJobs, required)
			return false, nil
		})
	require.NoError(t, err)
}

func ensureHostPathPVC(t *testing.T, pvcName string) {
	kubeConfig, err := framework.NewClientConfigForTest("")
	require.NoError(t, err)

	c, err := corev1client.NewForConfig(kubeConfig)
	require.NoError(t, err)

	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName + "-pv",
		},
		Spec: corev1.PersistentVolumeSpec{
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

	_, err = c.PersistentVolumes().Get(context.Background(), pv.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			_, err = c.PersistentVolumes().Create(context.Background(), &pv, metav1.CreateOptions{})
			require.NoErrorf(t, err, "could not create PV")
		} else {
			require.NoErrorf(t, err, "could not retrieve PV")
		}
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

func ensureCronJobRemoved(t *testing.T, name string) {
	c := framework.NewBatchClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		_, err := c.CronJobs(OpenShiftEtcdNamespace).Get(context.Background(), name, metav1.GetOptions{})
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

func ensureEtcdBackupsRemoved(t *testing.T, name string) {
	c := framework.NewOperatorClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
		backupsList, err := c.OperatorV1alpha1().EtcdBackups().List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.Infof("error while getting cronjob, waiting for its deletion: %v", err)
			return false, nil
		}

		for _, b := range backupsList.Items {
			klog.Infof("EnsureBackupRemoval found backup with name: %s", b.Name)
			if strings.Contains(b.Name, name) {
				return false, nil
			}
		}

		return true, nil
	})
	require.NoErrorf(t, err, "waiting for etcdbackups deletion error")
}

func ensureAllBackupPodsAreRemoved(t *testing.T) {
	c := framework.NewCoreClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (bool, error) {
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
	conditionType operatorv1alpha1.BackupConditionReason,
	status metav1.ConditionStatus) bool {

	for _, condition := range backup.Status.Conditions {
		if string(conditionType) != condition.Type {
			continue
		}
		return condition.Status == status
	}
	return false
}

func collectFilesInPVCAcrossAllNodes(t *testing.T, pvcName string) []string {
	client := framework.NewCoreClient(t)

	list, err := client.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/master"})
	require.NoErrorf(t, err, "error while listing nodes")

	// we will get empty strings and "." returned from find that we want to deduplicate here
	var lines []string
	linesSet := make(map[string]bool)
	for _, node := range list.Items {
		pvcLines := listFilesInPVC(t, pvcName, node)
		for _, l := range pvcLines {
			if _, ok := linesSet[l]; !ok {
				linesSet[l] = true
				lines = append(lines, l)
			}
		}
	}
	return lines
}

func listFilesInPVC(t *testing.T, pvcName string, node corev1.Node) []string {
	client := framework.NewCoreClient(t)
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.SimpleNameGenerator.GenerateName("backup-finder-pod" + "-"),
			Namespace: OpenShiftEtcdNamespace,
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "backup-dir",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:       "finder",
					Image:      ShellImage,
					Command:    []string{"find", "."},
					WorkingDir: HostPathBasePath,
					Resources:  corev1.ResourceRequirements{},
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
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": node.Name,
			},
			Tolerations: []corev1.Toleration{{Operator: "Exists"}},
		},
	}

	_, err := client.Pods(OpenShiftEtcdNamespace).Create(context.Background(), &pod, metav1.CreateOptions{})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err = wait.PollUntilContextCancel(ctx, 10*time.Second, false, func(ctx context.Context) (done bool, err error) {
		p, err := client.Pods(OpenShiftEtcdNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			klog.Infof("error while getting finder pod: %v", err)
			return false, nil
		}

		if p.Status.Phase == corev1.PodFailed {
			return true, fmt.Errorf("finder pod failed with status: %s", p.Status.String())
		}

		return p.Status.Phase == corev1.PodSucceeded, nil
	})
	require.NoErrorf(t, err, "waiting for finder pod error")

	logBytes, err := client.Pods(OpenShiftEtcdNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Do(ctx).Raw()
	files := strings.Split(string(logBytes), "\n")
	klog.Infof("found files on node [%s]: %v", node.Name, files)
	return files
}

func requireBackupFilesFound(t *testing.T, name string, files []string) {
	// a successful backup will look like this:
	// ./backup-backup-happy-path-2023-08-03_152313
	// ./backup-backup-happy-path-2023-08-03_152313/static_kuberesources_2023-08-03_152316__POSSIBLY_DIRTY__.tar.gz
	// ./backup-backup-happy-path-2023-08-03_152313/snapshot_2023-08-03_152316__POSSIBLY_DIRTY__.db ]

	// we assert that there are always at least two files:
	tarMatchFound := false
	snapMatchFound := false
	for _, file := range files {
		matchesTar, err := regexp.MatchString(`\./backup-`+name+`-.*.tar.gz`, file)
		require.NoError(t, err)
		if matchesTar {
			klog.Infof("Found matching kube resources: %s", file)
			tarMatchFound = true
		}

		matchesSnap, err := regexp.MatchString(`\./backup-`+name+`-.*/snapshot_.*.db`, file)
		require.NoError(t, err)
		if matchesSnap {
			klog.Infof("Found matching snapshot: %s", file)
			snapMatchFound = true
		}
	}

	require.Truef(t, tarMatchFound, "expected tarfile for backup: %s, found files: %v ", name, files)
	require.Truef(t, snapMatchFound, "expected snapshot for backup: %s, found files: %v ", name, files)
}

func groupBackupFilesByRunPrefix(t *testing.T, files []string) map[string][]string {
	// find will return a dir prefix always before any files like this:
	// ./backup-backup-happy-path-2023-08-03_152313
	// which is what we're going to use to group on
	groupingPrefixes := make(map[string][]string)

	for _, file := range files {
		if strings.Count(file, "/") == 1 {
			groupingPrefixes[file] = []string{}
		}
	}

	for _, file := range files {
		for k := range groupingPrefixes {
			if strings.HasPrefix(file, k) {
				groupingPrefixes[k] = append(groupingPrefixes[k], file)
				break
			}
		}
	}

	return groupingPrefixes
}

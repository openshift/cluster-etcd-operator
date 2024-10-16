package periodicbackupcontroller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/labels"

	corev1 "k8s.io/api/core/v1"

	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"

	configv1 "github.com/openshift/api/config/v1"
	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	fake "github.com/openshift/client-go/config/clientset/versioned/fake"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var backupFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
	[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName},
	[]configv1.FeatureGateName{})

func TestSyncLoopHappyPath(t *testing.T) {
	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	operatorFake := fake.NewClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewClientset()
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
		&operatorv1.StaticPodOperatorStatus{}, nil, nil)

	controller := PeriodicBackupController{
		operatorClient:        fakeOperatorClient,
		backupsClient:         operatorFake.ConfigV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupCronJobCreated(t, client, backup)
	requireOperatorStatus(t, fakeOperatorClient, false)
}

func TestSyncLoopWithDefaultBackupCR(t *testing.T) {
	var backups backupv1alpha1.BackupList

	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	// no default CR
	backups.Items = append(backups.Items, backup)
	operatorFake := fake.NewClientset([]runtime.Object{&backups}...)
	client := k8sfakeclient.NewClientset([]runtime.Object{defaultEtcdEndpointCM()}...)
	fakeKubeInformerForNamespace := v1helpers.NewKubeInformersForNamespaces(client, operatorclient.TargetNamespace)
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
		&operatorv1.StaticPodOperatorStatus{}, nil, nil)

	controller := PeriodicBackupController{
		operatorClient:        fakeOperatorClient,
		podLister:             fakeKubeInformerForNamespace.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		backupsClient:         operatorFake.ConfigV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "pullspec-image",
		backupVarGetter:       backuphelpers.NewDisabledBackupConfig(),
		featureGateAccessor:   backupFeatureGateAccessor,
		kubeInformers:         fakeKubeInformerForNamespace,
	}

	stopChan := make(chan struct{})
	t.Cleanup(func() {
		close(stopChan)
	})
	fakeKubeInformerForNamespace.Start(stopChan)

	expDisabledBackupVar := "    args:\n    - --enabled=false"
	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	require.Equal(t, expDisabledBackupVar, controller.backupVarGetter.ArgString())

	// create default CR
	defaultBackup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: defaultBackupCRName},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "0 */2 * * *",
				TimeZone: "GMT",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 3}}}}}

	backups.Items = append(backups.Items, defaultBackup)
	operatorFake = fake.NewClientset([]runtime.Object{&backups}...)
	controller.backupsClient = operatorFake.ConfigV1alpha1()

	err = controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	act, err := controller.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Get(context.TODO(), backupServerDaemonSet, v1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(act.Spec.Template.Spec.Containers))
	require.Equal(t, etcdBackupServerContainerName, act.Spec.Template.Spec.Containers[0].Name)
	require.Equal(t, []string{"cluster-etcd-operator", "backup-server"}, act.Spec.Template.Spec.Containers[0].Command)
	endpoints, err := getEtcdEndpoints(context.TODO(), controller.kubeClient)
	require.NoError(t, err)
	slices.Sort(endpoints)
	require.Equal(t, constructBackupServerArgs(defaultBackup, strings.Join(endpoints, ",")), act.Spec.Template.Spec.Containers[0].Args)

	// removing defaultCR
	backups.Items = backups.Items[:len(backups.Items)-1]
	operatorFake = fake.NewClientset([]runtime.Object{&backups}...)
	controller.backupsClient = operatorFake.ConfigV1alpha1()

	err = controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	_, err = controller.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Get(context.TODO(), backupServerDaemonSet, v1.GetOptions{})
	require.Error(t, err)
	require.Equal(t, fmt.Errorf("daemonsets.apps \"backup-server-daemon-set\" not found").Error(), err.Error())
}

func TestSyncLoopFailsDegradesOperatorWithDefaultBackupCR(t *testing.T) {
	var backups backupv1alpha1.BackupList

	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	backupServerFailureMsg := fmt.Sprintf("error running etcd backup: %s", "error running backup")
	client := k8sfakeclient.NewClientset([]runtime.Object{
		etcdBackupServerFailingPod("1", backupServerFailureMsg),
		etcdBackupServerFailingPod("2", backupServerFailureMsg),
		etcdBackupServerFailingPod("3", backupServerFailureMsg),
		defaultEtcdEndpointCM()}...)

	fakeKubeInformerForNamespace := v1helpers.NewKubeInformersForNamespaces(client, operatorclient.TargetNamespace)

	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
		&operatorv1.StaticPodOperatorStatus{}, nil, nil)

	// no default CR
	backups.Items = append(backups.Items, backup)
	operatorFake := fake.NewClientset([]runtime.Object{&backups}...)

	controller := PeriodicBackupController{
		operatorClient:        fakeOperatorClient,
		podLister:             fakeKubeInformerForNamespace.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		backupsClient:         operatorFake.ConfigV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "pullspec-image",
		backupVarGetter:       backuphelpers.NewDisabledBackupConfig(),
		featureGateAccessor:   backupFeatureGateAccessor,
		kubeInformers:         fakeKubeInformerForNamespace,
	}

	stopChan := make(chan struct{})
	t.Cleanup(func() {
		close(stopChan)
	})
	fakeKubeInformerForNamespace.Start(stopChan)

	expDisabledBackupVar := "    args:\n    - --enabled=false"
	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	require.Equal(t, expDisabledBackupVar, controller.backupVarGetter.ArgString())
	requireOperatorStatus(t, fakeOperatorClient, false)

	// create default CR
	defaultBackup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: defaultBackupCRName},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "0 */2 * * *",
				TimeZone: "GMT",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 3}}}}}

	backups.Items = append(backups.Items, defaultBackup)
	operatorFake = fake.NewClientset([]runtime.Object{&backups}...)
	controller.backupsClient = operatorFake.ConfigV1alpha1()

	err = controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	act, err := controller.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Get(context.TODO(), backupServerDaemonSet, v1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, len(act.Spec.Template.Spec.Containers))
	require.Equal(t, etcdBackupServerContainerName, act.Spec.Template.Spec.Containers[0].Name)
	require.Equal(t, []string{"cluster-etcd-operator", "backup-server"}, act.Spec.Template.Spec.Containers[0].Command)
	endpoints, err := getEtcdEndpoints(context.TODO(), controller.kubeClient)
	require.NoError(t, err)
	slices.Sort(endpoints)
	require.Equal(t, constructBackupServerArgs(defaultBackup, strings.Join(endpoints, ",")), act.Spec.Template.Spec.Containers[0].Args)
	requireOperatorStatus(t, fakeOperatorClient, true)

	// removing defaultCR
	backups.Items = backups.Items[:len(backups.Items)-1]
	operatorFake = fake.NewClientset([]runtime.Object{&backups}...)
	controller.backupsClient = operatorFake.ConfigV1alpha1()

	err = controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	_, err = controller.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Get(context.TODO(), backupServerDaemonSet, v1.GetOptions{})
	require.Error(t, err)
	require.Equal(t, fmt.Errorf("daemonsets.apps \"backup-server-daemon-set\" not found").Error(), err.Error())
	requireOperatorStatus(t, fakeOperatorClient, false)
}

func TestSyncLoopExistingCronJob(t *testing.T) {
	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	cronJob := batchv1.CronJob{ObjectMeta: v1.ObjectMeta{Name: "test-backup", Namespace: operatorclient.TargetNamespace}}
	operatorFake := fake.NewClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewClientset([]runtime.Object{&cronJob}...)
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
		&operatorv1.StaticPodOperatorStatus{}, nil, nil)

	controller := PeriodicBackupController{
		operatorClient:        fakeOperatorClient,
		backupsClient:         operatorFake.ConfigV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NoError(t, err)
	requireBackupCronJobUpdated(t, client, backup)
	requireOperatorStatus(t, fakeOperatorClient, false)
}

func TestSyncLoopFailsDegradesOperator(t *testing.T) {
	backup := backupv1alpha1.Backup{ObjectMeta: v1.ObjectMeta{Name: "test-backup"},
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "20 4 * * *",
				TimeZone: "UTC",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 5}},
				PVCName: "backup-happy-path-pvc"}}}

	operatorFake := fake.NewClientset([]runtime.Object{&backup}...)
	client := k8sfakeclient.NewClientset()
	client.Fake.PrependReactor("create", "cronjobs", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, fmt.Errorf("could not create cronjob")
	})

	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
		&operatorv1.StaticPodOperatorStatus{}, nil, nil)

	controller := PeriodicBackupController{
		operatorClient:        fakeOperatorClient,
		backupsClient:         operatorFake.ConfigV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: "pullspec-image",
		featureGateAccessor:   backupFeatureGateAccessor,
	}

	err := controller.sync(context.TODO(), nil)
	require.NotNil(t, err)
	require.Equal(t, "PeriodicBackupController could not reconcile backup [test-backup] with cronjob: PeriodicBackupController could not create cronjob test-backup: could not create cronjob", err.Error())
	requireOperatorStatus(t, fakeOperatorClient, true)
}

func TestBackupRetentionCommand(t *testing.T) {
	testCases := map[string]struct {
		policy       backupv1alpha1.RetentionPolicy
		expectedArgs []string
	}{
		"none default": {
			policy:       backupv1alpha1.RetentionPolicy{},
			expectedArgs: []string{"prune-backups", "--type=RetentionNumber", "--maxNumberOfBackups=15"},
		},
		"none, number set": {
			policy:       backupv1alpha1.RetentionPolicy{RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 13}},
			expectedArgs: []string{"prune-backups", "--type=RetentionNumber", "--maxNumberOfBackups=13"},
		},
		"number type, number set": {
			policy: backupv1alpha1.RetentionPolicy{
				RetentionType:   backupv1alpha1.RetentionTypeNumber,
				RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 42}},
			expectedArgs: []string{"prune-backups", "--type=RetentionNumber", "--maxNumberOfBackups=42"},
		},
		"size type, size default": {
			policy: backupv1alpha1.RetentionPolicy{
				RetentionType: backupv1alpha1.RetentionTypeSize,
				RetentionSize: &backupv1alpha1.RetentionSizeConfig{MaxSizeOfBackupsGb: 10}},
			expectedArgs: []string{"prune-backups", "--type=RetentionSize", "--maxSizeOfBackupsGb=10"},
		},
		"size type, size set": {
			policy: backupv1alpha1.RetentionPolicy{
				RetentionType: backupv1alpha1.RetentionTypeSize,
				RetentionSize: &backupv1alpha1.RetentionSizeConfig{MaxSizeOfBackupsGb: 44}},
			expectedArgs: []string{"prune-backups", "--type=RetentionSize", "--maxSizeOfBackupsGb=44"},
		},
	}

	for k, v := range testCases {
		t.Run(k, func(t *testing.T) {
			c, _ := newCronJob()
			require.NoError(t, setRetentionPolicyInitContainer(v.policy, c, "image"))
			require.Equal(t, "image", c.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Image)
			require.Equal(t, []string{"cluster-etcd-operator"}, c.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Command)
			require.Equal(t, v.expectedArgs, c.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Args)
		})
	}
}

func TestBackupRetentionCommandUnknownType(t *testing.T) {
	c, _ := newCronJob()
	err := setRetentionPolicyInitContainer(backupv1alpha1.RetentionPolicy{RetentionType: "something"}, c, "image")
	require.Equal(t, fmt.Errorf("unknown retention type: something"), err)
}

func TestCronJobSpecDiffs(t *testing.T) {
	job, err := newCronJob()
	require.NoError(t, err)
	require.False(t, cronSpecDiffers(job.Spec, job.Spec))

	job2 := job.DeepCopy()
	job2.Spec.Schedule = "something else"
	require.True(t, cronSpecDiffers(job.Spec, job2.Spec))
}

func TestConstructBackupServerArgs(t *testing.T) {
	testEtcdEndpoints := "10.0.109.40:2379,10.0.63.58:2379,10.0.44.255:2379"
	testCR := backupv1alpha1.Backup{
		Spec: backupv1alpha1.BackupSpec{
			EtcdBackupSpec: backupv1alpha1.EtcdBackupSpec{
				Schedule: "* * * * *",
				TimeZone: "GMT",
				RetentionPolicy: backupv1alpha1.RetentionPolicy{
					RetentionType:   backupv1alpha1.RetentionTypeNumber,
					RetentionNumber: &backupv1alpha1.RetentionNumberConfig{MaxNumberOfBackups: 3},
				},
			},
		},
	}

	exp := []string{"--enabled=true", "--timezone=GMT", "--schedule=* * * * *", "--type=RetentionNumber", "--maxNumberOfBackups=3", "--endpoints=10.0.109.40:2379,10.0.63.58:2379,10.0.44.255:2379", "--backupPath=/var/lib/etcd-auto-backup"}

	act := constructBackupServerArgs(testCR, testEtcdEndpoints)
	require.Equal(t, exp, act)
}

func TestGetEtcdEndpoints(t *testing.T) {
	testEtcdEndpointCM := defaultEtcdEndpointCM()

	exp := []string{"10.0.0.0:2379", "10.0.0.1:2379", "10.0.0.2:2379"}

	client := k8sfakeclient.NewClientset([]runtime.Object{testEtcdEndpointCM}...)
	act, err := getEtcdEndpoints(context.TODO(), client)
	require.NoError(t, err)
	require.ElementsMatch(t, exp, act)
}

func requireOperatorStatus(t *testing.T, client v1helpers.StaticPodOperatorClient, degraded bool) {
	_, status, _, _ := client.GetOperatorState()
	require.Equal(t, 1, len(status.Conditions))
	require.Equal(t, "PeriodicBackupControllerDegraded", status.Conditions[0].Type)
	if degraded {
		require.Equal(t, operatorv1.ConditionTrue, status.Conditions[0].Status)

	} else {
		require.Equal(t, operatorv1.ConditionFalse, status.Conditions[0].Status)
	}
}

func requireBackupCronJobCreated(t *testing.T, client *k8sfakeclient.Clientset, backup backupv1alpha1.Backup) {
	createAction := findFirstCreateAction(client)
	require.NotNilf(t, createAction, "expected to find at least one createAction, but found %v", client.Fake.Actions())
	require.Equal(t, operatorclient.TargetNamespace, createAction.GetNamespace())
	require.Equal(t, "create", createAction.GetVerb())
	createdCronJob := createAction.Object.(*batchv1.CronJob)

	require.Equal(t, backup.Name, createdCronJob.Name)
	require.Equal(t, backup.Spec.EtcdBackupSpec.Schedule, createdCronJob.Spec.Schedule)
	require.Equal(t, backup.Spec.EtcdBackupSpec.TimeZone, *createdCronJob.Spec.TimeZone)
	require.Equal(t, operatorclient.TargetNamespace, createdCronJob.Namespace)
	require.Equal(t, "pullspec-image", createdCronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image)
	require.Equal(t, 1, len(createdCronJob.OwnerReferences))
	require.Equal(t, v1.OwnerReference{Name: backup.Name}, createdCronJob.OwnerReferences[0])
	require.Equal(t, "pullspec-image", createdCronJob.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Image)
	require.Contains(t, createdCronJob.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Args, "prune-backups")
	require.Contains(t, createdCronJob.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Args, fmt.Sprintf("--type=%s", backup.Spec.EtcdBackupSpec.RetentionPolicy.RetentionType))
}

func requireBackupCronJobUpdated(t *testing.T, client *k8sfakeclient.Clientset, backup backupv1alpha1.Backup) {
	var updateAction *k8stesting.UpdateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.UpdateActionImpl); ok {
			updateAction = &a
			break
		}
	}

	require.NotNilf(t, updateAction, "expected to find at least one updateAction, but found %v", client.Fake.Actions())
	j := updateAction.Object.(*batchv1.CronJob)
	require.Equal(t, backup.Name, j.Name)
	require.Equal(t, backup.Spec.EtcdBackupSpec.Schedule, j.Spec.Schedule)
	require.Equal(t, backup.Spec.EtcdBackupSpec.TimeZone, *j.Spec.TimeZone)
}

func findFirstCreateAction(client *k8sfakeclient.Clientset) *k8stesting.CreateActionImpl {
	var createAction *k8stesting.CreateActionImpl
	for _, action := range client.Fake.Actions() {
		if a, ok := action.(k8stesting.CreateActionImpl); ok {
			createAction = &a
			break
		}
	}
	return createAction
}

func defaultEtcdEndpointCM() *corev1.ConfigMap {
	return u.FakeConfigMap(operatorclient.TargetNamespace, etcdEndpointConfigMapName, map[string]string{
		"0": "10.0.0.0",
		"1": "10.0.0.1",
		"2": "10.0.0.2",
	})
}

func etcdBackupServerFailingPod(nodeName string, failureMsg string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      fmt.Sprintf("etcd-backup-server-%v", nodeName),
			Namespace: "openshift-etcd",
			Labels:    labels.Set{backupDSLabelKey: backupDSLabelValue},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: etcdBackupServerContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Message: failureMsg,
					},
				}},
			},
		},
	}
}

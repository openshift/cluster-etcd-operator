package backuppolicycontroller

import (
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	k8sfakeclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	testingclock "k8s.io/utils/clock/testing"
)

var backupFeatureGateAccessor = featuregates.NewHardcodedFeatureGateAccess(
	[]configv1.FeatureGateName{backuphelpers.AutomatedEtcdBackupFeatureGateName},
	[]configv1.FeatureGateName{})

type testCaseBackupPolicyController struct {
	backupPolicies []*operatorv1alpha1.EtcdBackupPolicy
	cronJobs       []*batchv1.CronJob
	expectError    bool
	validate       func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset)
}

const fakeOperatorImagePullSpec = "pullspec-image"

func runBackupPolicyControllerTest(t *testing.T, tc testCaseBackupPolicyController) {
	t.Helper()
	operatorObjs := make([]runtime.Object, 0, len(tc.backupPolicies))
	operatorObjs = testutils.AppendRuntimeObjects(operatorObjs, tc.backupPolicies)

	k8sObjs := make([]runtime.Object, 0, len(tc.cronJobs))
	k8sObjs = testutils.AppendRuntimeObjects(k8sObjs, tc.cronJobs)

	client := k8sfakeclient.NewSimpleClientset(k8sObjs...)
	operatorFake := operatorfake.NewSimpleClientset(operatorObjs...)

	sharedFactory := informers.NewSharedInformerFactory(client, 0)
	operatorSharedFactory := operatorinformers.NewSharedInformerFactory(operatorFake, 0)

	backupPoliciesInformer := operatorSharedFactory.Operator().V1alpha1().EtcdBackupPolicies()
	cronJobsInformer := sharedFactory.Batch().V1().CronJobs()

	backupPoliciesInformerHasSynced := backupPoliciesInformer.Informer().HasSynced
	cronJobsInformerHasSynced := cronJobsInformer.Informer().HasSynced

	ctx := t.Context()
	sharedFactory.Start(ctx.Done())
	operatorSharedFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), cronJobsInformerHasSynced, backupPoliciesInformerHasSynced)

	fakeClock := testingclock.NewFakeClock(time.Now())
	eventRecorder := events.NewInMemoryRecorder("test", fakeClock)
	t.Cleanup(eventRecorder.Shutdown)

	controller := &BackupPolicyController{
		backupPoliciesLister:  backupPoliciesInformer.Lister(),
		cronJobLister:         cronJobsInformer.Lister().CronJobs(operatorclient.TargetNamespace),
		operatorClient:        operatorFake.OperatorV1alpha1(),
		kubeClient:            client,
		operatorImagePullSpec: fakeOperatorImagePullSpec,
		featureGateAccessor:   backupFeatureGateAccessor,
		eventRecorder:         eventRecorder,
		cronParser:            cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
	}

	for _, backupPolicy := range tc.backupPolicies {
		syncCtx := testutils.FakeSyncContext(t, backupPolicy.Name)
		err := controller.sync(ctx, syncCtx)
		if tc.expectError {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}

	if tc.validate != nil {
		tc.validate(t, client, operatorFake)
	}
}

func TestBackupPolicyCreateCronJob(t *testing.T) {
	runBackupPolicyControllerTest(t, testCaseBackupPolicyController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{
			testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Verify new cronjob was created
			action, ok := testutils.GetAction[k8stesting.CreateActionImpl](client.Fake.Actions())
			require.True(t, ok, "expected create action")
			cronJob, ok := action.Object.(*batchv1.CronJob)
			require.True(t, ok, "expected cronjob to be created")

			require.Equal(t, "test-backup-policy", cronJob.Labels[backuphelpers.LabelEtcdBackupPolicy])
			require.Equal(t, "@daily", cronJob.Spec.Schedule)
		},
	})
}

func TestBackupPolicyExistingCronJob(t *testing.T) {
	backupPolicy := testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))
	cronJob, err := newCronJob(backupPolicy, fakeOperatorImagePullSpec)
	require.NoError(t, err)
	runBackupPolicyControllerTest(t, testCaseBackupPolicyController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{backupPolicy},
		cronJobs:       []*batchv1.CronJob{cronJob},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Verify no cronjob was created or updated
			_, ok := testutils.GetAction[k8stesting.CreateActionImpl](client.Fake.Actions())
			require.False(t, ok, "expected no create action")
			_, ok = testutils.GetAction[k8stesting.UpdateActionImpl](client.Fake.Actions())
			require.False(t, ok, "expected no update action")

			// Verify cronjob still exists
			_, err := client.BatchV1().CronJobs(operatorclient.TargetNamespace).Get(t.Context(), "test-backup-policy", v1.GetOptions{})
			require.NoError(t, err)
		},
	})
}

func TestBackupPolicyUpdateCronJob(t *testing.T) {
	backupPolicy := testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))
	runBackupPolicyControllerTest(t, testCaseBackupPolicyController{
		backupPolicies: []*operatorv1alpha1.EtcdBackupPolicy{backupPolicy},
		cronJobs: []*batchv1.CronJob{{ObjectMeta: v1.ObjectMeta{
			Name:      backupPolicy.Name,
			Namespace: operatorclient.TargetNamespace,
		}}},
		validate: func(t *testing.T, client *k8sfakeclient.Clientset, operatorFake *operatorfake.Clientset) {
			// Verify no cronjob was created or updated
			_, ok := testutils.GetAction[k8stesting.CreateActionImpl](client.Fake.Actions())
			require.False(t, ok, "expected no create action")
			action, ok := testutils.GetAction[k8stesting.UpdateActionImpl](client.Fake.Actions())
			require.True(t, ok, "expected update action")
			cronJob, ok := action.Object.(*batchv1.CronJob)
			require.True(t, ok, "expected cronjob to be updated")

			expectedCronJob, err := newCronJob(backupPolicy, fakeOperatorImagePullSpec)
			require.NoError(t, err)
			require.Equal(t, expectedCronJob, cronJob)

			require.Equal(t, "test-backup-policy", cronJob.Labels[backuphelpers.LabelEtcdBackupPolicy])
			require.Equal(t, "@daily", cronJob.Spec.Schedule)
		},
	})
}

func TestCronJobSpecDiffs(t *testing.T) {
	backupPolicy := testutils.FakeEtcdBackupPolicy("test-backup-policy", "@daily", testutils.WithBackupPolicyAge(25*time.Hour))
	job, err := newCronJob(backupPolicy, fakeOperatorImagePullSpec)
	require.NoError(t, err)
	require.False(t, cronSpecDiffers(job.Spec, job.Spec))

	job2 := job.DeepCopy()
	job2.Spec.Schedule = "something else"
	require.True(t, cronSpecDiffers(job.Spec, job2.Spec))
}

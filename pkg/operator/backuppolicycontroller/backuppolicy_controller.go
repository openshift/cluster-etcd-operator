package backuppolicycontroller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	batchv1listers "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	jobFinishedTTL     = 4 * time.Hour
	specHashAnnotation = "operator.openshift.io/spec-hash"
)

type BackupPolicyController struct {
	backupPoliciesLister  operatorv1alpha1listers.EtcdBackupPolicyLister
	cronJobLister         batchv1listers.CronJobNamespaceLister
	operatorClient        operatorv1alpha1client.OperatorV1alpha1Interface
	kubeClient            kubernetes.Interface
	operatorImagePullSpec string
	featureGateAccessor   featuregates.FeatureGateAccess
	eventRecorder         events.Recorder
	cronParser            cron.Parser
}

func NewBackupPolicyController(
	livenessChecker *health.MultiAlivenessChecker,
	backupPoliciesLister operatorv1alpha1listers.EtcdBackupPolicyLister,
	cronJobLister batchv1listers.CronJobNamespaceLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	kubeClient kubernetes.Interface,
	staticPodOperatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	etcdBackupPolicyInformer factory.Informer,
	cronJobInformer cache.SharedIndexInformer) factory.Controller {

	c := &BackupPolicyController{
		backupPoliciesLister:  backupPoliciesLister,
		cronJobLister:         cronJobLister,
		operatorClient:        operatorClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		featureGateAccessor:   accessor,
		eventRecorder:         eventRecorder.WithComponentSuffix("backup-policy-controller"),
		cronParser:            cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("BackupPolicyController", syncer)

	return factory.New().
		WithInformersQueueKeysFunc(
			func(o runtime.Object) []string {
				if backupPolicy, ok := o.(*operatorv1alpha1.EtcdBackupPolicy); ok {
					return []string{backupPolicy.Name}
				}
				if cronJob, ok := o.(*batchv1.CronJob); ok && cronJob.Labels != nil {
					if backupPolicyName := cronJob.Labels[backuphelpers.LabelEtcdBackupPolicy]; backupPolicyName != "" {
						return []string{backupPolicyName}
					}
				}
				return nil
			},
			etcdBackupPolicyInformer,
			cronJobInformer,
		).
		WithSync(syncer.Sync).
		WithPostStartHooks(func(ctx context.Context, syncCtx factory.SyncContext) error {
			wait.UntilWithContext(ctx, func(ctx context.Context) {
				backupPolicies, err := c.backupPoliciesLister.List(labels.Everything())
				if err != nil {
					klog.Warningf("BackupPolicyController failed to list EtcdBackupPolicies for queueing: %s", err)
					updateControllerDegradedCondition(ctx, staticPodOperatorClient, operatorv1.ConditionTrue, "Error")
					return
				}

				for _, backupPolicy := range backupPolicies {
					syncCtx.Queue().Add(backupPolicy.Name)
				}
				updateControllerDegradedCondition(ctx, staticPodOperatorClient, operatorv1.ConditionFalse, "AsExpected")
			}, 1*time.Minute)
			return nil
		}).
		ToController("BackupPolicyController", eventRecorder.WithComponentSuffix("backup-policy-controller"))
}

func (c *BackupPolicyController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("BackupPolicyController error while checking feature flags: %v", err)
		}
		return nil
	}

	backupPolicyName := syncCtx.QueueKey()
	backupPolicy, err := c.backupPoliciesLister.Get(backupPolicyName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("BackupPolicyController could not get EtcdBackupPolicy %s: %w", backupPolicyName, err)
	} else if backupPolicy.DeletionTimestamp != nil {
		return nil
	}
	return c.reconcileCronJob(ctx, backupPolicy)
}

func (c *BackupPolicyController) reconcileCronJob(ctx context.Context, backupPolicy *operatorv1alpha1.EtcdBackupPolicy) error {
	cronJobClient := c.kubeClient.BatchV1().CronJobs(operatorclient.TargetNamespace)
	currentCronJob, err := c.cronJobLister.Get(backupPolicy.Name)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("BackupPolicyController could not get cronjob %s: %w", backupPolicy.Name, err)
		}
	}

	schedule := backupPolicy.Spec.Schedule
	if backupPolicy.Spec.TimeZone != "" {
		schedule = "TZ=" + backupPolicy.Spec.TimeZone + " " + schedule
	}
	if _, err := c.cronParser.Parse(schedule); err != nil {
		// Invalid schedule
		if currentCronJob != nil {
			if deleteErr := cronJobClient.Delete(ctx, backupPolicy.Name, v1.DeleteOptions{}); deleteErr != nil {
				if !apierrors.IsNotFound(deleteErr) {
					deleteErr = fmt.Errorf("BackupPolicyController failed to delete cronjob %s for invalid schedule: %w", backupPolicy.Name, deleteErr)
					return utilerrors.NewAggregate([]error{err, deleteErr})
				}
			}
		}
		return c.updateScheduledCondition(ctx, backupPolicy, operatorv1alpha1.BackupPolicyInvalidSchedule, err.Error(), v1.ConditionFalse)
	}

	cronJob, err := newCronJob(backupPolicy, c.operatorImagePullSpec)
	if err != nil {
		return err
	}

	if currentCronJob == nil {
		_, err := cronJobClient.Create(ctx, cronJob, v1.CreateOptions{})
		if err != nil {
			var updateErr error
			if !apierrors.IsConflict(err) {
				updateErr = c.updateScheduledCondition(ctx, backupPolicy, operatorv1alpha1.BackupPolicySchedulingFailed, "failed to create CronJob", v1.ConditionFalse)
			}
			return utilerrors.NewAggregate([]error{fmt.Errorf("BackupPolicyController failed to create cronjob %s: %w", cronJob.Name, err), updateErr})
		}
	} else {
		if cronSpecHashDiffers(cronJob, currentCronJob) {
			klog.V(4).Infof("detected diff in cronjob specs, updating cronjob: %s", cronJob.Name)
			_, err := cronJobClient.Update(ctx, cronJob, v1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("BackupPolicyController failed to update cronjob %s: %w", cronJob.Name, err)
			}
		}
	}

	return c.updateScheduledCondition(ctx, backupPolicy, operatorv1alpha1.BackupPolicySchedulingSucceeded, "backup policy successfully scheduled", v1.ConditionTrue)
}

func (c *BackupPolicyController) updateScheduledCondition(ctx context.Context, backupPolicy *operatorv1alpha1.EtcdBackupPolicy, reason operatorv1alpha1.EtcdBackupPolicyConditionReason, message string, status v1.ConditionStatus) error {
	conditions, requireUpdate := updateCondition(backupPolicy.Status.Conditions, v1.Condition{
		Type:               string(operatorv1alpha1.BackupPolicyScheduled),
		Reason:             string(reason),
		Message:            message,
		Status:             status,
		LastTransitionTime: v1.Now(),
		ObservedGeneration: backupPolicy.Generation,
	})
	if requireUpdate {
		backupPolicy = backupPolicy.DeepCopy()
		backupPolicy.Status.Conditions = conditions
		if _, err := c.operatorClient.EtcdBackupPolicies().UpdateStatus(ctx, backupPolicy, v1.UpdateOptions{}); err != nil {
			return fmt.Errorf("BackupPolicyController failed to update status for policy %q: %w", backupPolicy.Name, err)
		}
	}
	return nil
}

func newCronJob(backupPolicy *operatorv1alpha1.EtcdBackupPolicy, operatorImagePullSpec string) (*batchv1.CronJob, error) {
	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("BackupPolicyController could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), bindata.MustAsset("etcd/cluster-backup-cronjob.yaml"))
	if err != nil {
		return nil, fmt.Errorf("BackupPolicyController could not decode batchv1 job scheme: %w", err)
	}
	cronJob := obj.(*batchv1.CronJob)

	cronJob.Name = backupPolicy.Name
	cronJob.Labels[backuphelpers.LabelEtcdBackupPolicy] = backupPolicy.Name
	cronJob.OwnerReferences = append(cronJob.OwnerReferences, v1.OwnerReference{
		APIVersion: operatorv1alpha1.GroupVersion.String(),
		Kind:       "EtcdBackupPolicy",
		Name:       backupPolicy.Name,
		UID:        backupPolicy.UID,
	})

	cronJob.Spec.Schedule = backupPolicy.Spec.Schedule
	if backupPolicy.Spec.TimeZone != "" {
		cronJob.Spec.TimeZone = &backupPolicy.Spec.TimeZone
	}

	cronJob.Spec.JobTemplate.Spec.TTLSecondsAfterFinished = new(int32(jobFinishedTTL.Seconds()))
	cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec
	cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env = append(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{
			Name:  "ETCD_BACKUP_POLICY_NAME",
			Value: backupPolicy.Name,
		},
	)

	resourceapply.SetSpecHashAnnotation(&cronJob.ObjectMeta, cronJob.Spec)
	return cronJob, nil
}

func updateControllerDegradedCondition(ctx context.Context, operatorClient v1helpers.OperatorClient, status operatorv1.ConditionStatus, reason string) {
	_, _, updateErr := v1helpers.UpdateStatus(ctx, operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "BackupPolicyControllerDegraded",
		Status: status,
		Reason: reason,
	}))
	if updateErr != nil {
		klog.V(4).Infof("BackupPolicyController error during UpdateStatus: %v", updateErr)
	}
}

func updateCondition(conditions []v1.Condition, updated v1.Condition) ([]v1.Condition, bool) {
	idx := slices.IndexFunc(conditions, func(c v1.Condition) bool {
		return c.Type == updated.Type
	})
	if idx >= 0 {
		if conditionsDiffer(conditions[idx], updated) {
			conditions[idx] = updated
			return conditions, true
		}
		return conditions, false
	}
	conditions = append(conditions, updated)
	return conditions, true
}

func conditionsDiffer(a, b v1.Condition) bool {
	a.LastTransitionTime = v1.Time{}
	b.LastTransitionTime = v1.Time{}
	return a != b
}

func cronSpecHashDiffers(l *batchv1.CronJob, r *batchv1.CronJob) bool {
	if l.Annotations == nil || r.Annotations == nil {
		return true
	}
	return l.Annotations[specHashAnnotation] != r.Annotations[specHashAnnotation]
}

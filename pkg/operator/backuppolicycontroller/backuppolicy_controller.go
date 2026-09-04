package backuppolicycontroller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorv1alpha1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1alpha1"
	operatorv1alpha1listers "github.com/openshift/client-go/operator/listers/operator/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	maxNameLength = 63
)

type BackupPolicyController struct {
	backupsLister         operatorv1alpha1listers.EtcdBackupLister
	backupPoliciesLister  operatorv1alpha1listers.EtcdBackupPolicyLister
	nodeLister            corev1listers.NodeLister
	operatorClient        operatorv1alpha1client.OperatorV1alpha1Interface
	operatorImagePullSpec string
	featureGateAccessor   featuregates.FeatureGateAccess
	eventRecorder         events.Recorder
	cronParser            cron.Parser
}

func NewBackupPolicyController(
	livenessChecker *health.MultiAlivenessChecker,
	backupsLister operatorv1alpha1listers.EtcdBackupLister,
	backupPoliciesLister operatorv1alpha1listers.EtcdBackupPolicyLister,
	nodeLister corev1listers.NodeLister,
	operatorClient operatorv1alpha1client.OperatorV1alpha1Interface,
	staticPodOperatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	etcdBackupPolicyInformer factory.Informer,
	etcdBackupInformer factory.Informer,
	nodeInformer cache.SharedIndexInformer) factory.Controller {

	c := &BackupPolicyController{
		backupsLister:         backupsLister,
		backupPoliciesLister:  backupPoliciesLister,
		nodeLister:            nodeLister,
		operatorClient:        operatorClient,
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
				if backup, ok := o.(*operatorv1alpha1.EtcdBackup); ok {
					// Only trigger sync from backups when they are completed or failed
					if backupPolicyName := backup.Labels[backuphelpers.LabelEtcdBackupPolicy]; backupPolicyName != "" && backuphelpers.IsBackupFinished(backup) {
						return []string{backupPolicyName}
					}
				}
				return nil
			},
			etcdBackupPolicyInformer,
			etcdBackupInformer,
		).
		WithBareInformers(
			nodeInformer,
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

	if len(backupPolicy.Status.Active) > 0 {
		backupPolicy, err = c.syncActive(ctx, backupPolicy)
		if err != nil {
			return err
		} else if len(backupPolicy.Status.Active) > 0 {
			return nil
		}
	}

	schedule, err := c.parseSchedule(backupPolicy)
	if err != nil {
		return fmt.Errorf("BackupPolicyController failed to parse %s schedule: %w", backupPolicyName, err)
	}
	scheduleTime, err := nextScheduleTime(backupPolicy, time.Now(), schedule)
	if err != nil {
		return fmt.Errorf("BackupPolicyController failed to calculate next schedule time for EtcdBackupPolicy %s: %w", backupPolicy.Name, err)
	}

	if scheduleTime != nil {
		if err := c.executeBackup(ctx, backupPolicy, *scheduleTime); err != nil {
			return fmt.Errorf("BackupPolicyController failed to execute backup for EtcdBackupPolicy %s: %w", backupPolicy.Name, err)
		}
	}
	return nil
}

// parseSchedule parses the cron schedule with timezone support
func (c *BackupPolicyController) parseSchedule(backupPolicy *operatorv1alpha1.EtcdBackupPolicy) (cron.Schedule, error) {
	spec := backupPolicy.Spec

	schedule := spec.Schedule
	if spec.TimeZone != "" {
		schedule = fmt.Sprintf("TZ=%s %s", spec.TimeZone, spec.Schedule)
	}

	return c.cronParser.Parse(schedule)
}

func (c *BackupPolicyController) hasActiveBackup(ctx context.Context, backupPolicy *operatorv1alpha1.EtcdBackupPolicy) bool {
	// Live API call to ensure no EtcdBackups are missed
	backupList, err := c.operatorClient.EtcdBackups().List(ctx, v1.ListOptions{
		LabelSelector: backuphelpers.LabelEtcdBackupPolicy + "=" + backupPolicy.Name,
	})
	if err != nil {
		klog.V(4).Infof("BackupPolicyController failed to list EtcdBackups for EtcdBackupPolicy %s: %s", backupPolicy.Name, err)
		return true
	}

	for _, backup := range backupList.Items {
		if !backuphelpers.IsBackupFinished(&backup) {
			return true
		}
	}

	return false
}

func (c *BackupPolicyController) syncActive(ctx context.Context, backupPolicy *operatorv1alpha1.EtcdBackupPolicy) (*operatorv1alpha1.EtcdBackupPolicy, error) {
	backups, err := c.backupsLister.List(labels.SelectorFromSet(labels.Set{backuphelpers.LabelEtcdBackupPolicy: backupPolicy.Name}))
	if err != nil {
		return backupPolicy, fmt.Errorf("BackupPolicyController failed to list EtcdBackups: %w", err)
	}
	backupPolicy = backupPolicy.DeepCopy()

	activeMap := map[types.UID]string{}
	for _, backup := range backups {
		if !backuphelpers.IsBackupFinished(backup) {
			activeMap[backup.UID] = backup.Name
		}
	}

	active := backupPolicy.Status.Active[:0]
	for _, backupRef := range backupPolicy.Status.Active {
		uid := types.UID(backupRef.UID)
		if activeMap[uid] == backupRef.Name {
			delete(activeMap, uid)
			active = append(active, backupRef)
		}
	}

	updateStatus := len(active) != len(backupPolicy.Status.Active) || len(activeMap) > 0
	for uid, name := range activeMap {
		klog.Warningf("BackupPolicyController saw unexepected backup that the controller didn't create or it forgot: %s", name)
		active = append(active, operatorv1alpha1.EtcdBackupReference{Name: name, UID: string(uid)})
	}
	backupPolicy.Status.Active = active

	if updateStatus {
		if backupPolicy, err = c.operatorClient.EtcdBackupPolicies().UpdateStatus(ctx, backupPolicy, v1.UpdateOptions{}); err != nil {
			return backupPolicy, fmt.Errorf("BackupPolicyController failed to update EtcdBackupPolicy active backups: %w", err)
		}
	}

	return backupPolicy, nil
}

// executeBackup creates EtcdBackup resources for each master node
func (c *BackupPolicyController) executeBackup(ctx context.Context, backupPolicy *operatorv1alpha1.EtcdBackupPolicy, scheduleTime time.Time) error {
	// If any backups for this policy are currently active, then we skip this execution
	if c.hasActiveBackup(ctx, backupPolicy) {
		return nil
	}

	// Get master nodes
	var selector labels.Selector
	if len(backupPolicy.Spec.NodeSelector) != 0 {
		selector = labels.SelectorFromSet(backupPolicy.Spec.NodeSelector)
	}
	masterNodes, err := backuphelpers.SelectBackupNodes(c.nodeLister, selector)
	if err != nil {
		return fmt.Errorf("BackupPolicyController failed to select master nodes for backup: %w", err)
	}
	if len(masterNodes) == 0 {
		c.eventRecorder.Warningf("BackupExecutionSkipped",
			"No master nodes found for backup %s, skipping this execution", backupPolicy.Name)
		// TODO: Retry backoff?
		return nil
	}
	if backupPolicy.Spec.Storage.Type == operatorv1alpha1.EtcdBackupStorageTypePVC {
		// TODO(bhperry): Ideally the decision would be left up to the queue controller, since it can
		// 	more intelligently schedule the backup to a node. But EtcdBackup doesn't have a NodeSelector.
		//  Should both types have NodeName and NodeSelector?
		masterNodes = masterNodes[:1]
	}

	// Track failed creations
	failedCreations := []string{}

	// Create EtcdBackup for each selected master node
	etcdBackupsClient := c.operatorClient.EtcdBackups()
	active := make([]operatorv1alpha1.EtcdBackupReference, 0, len(masterNodes))
	for _, node := range masterNodes {
		// Deterministic naming to prevent duplicate EtcdBackups from stale informers
		backupName := generateEtcdBackupName(backupPolicy.Name, node.UID, scheduleTime)
		etcdBackup := &operatorv1alpha1.EtcdBackup{
			ObjectMeta: v1.ObjectMeta{
				Name: backupName,
				Labels: map[string]string{
					backuphelpers.LabelEtcdBackupPolicy: backupPolicy.Name,
				},
				Finalizers: []string{
					backuphelpers.FinalizerEtcdBackup,
				},
			},
			Spec: operatorv1alpha1.EtcdBackupSpec{
				NodeName: node.Name,
				Storage:  backupPolicy.Spec.Storage,
			},
		}

		if backup, err := etcdBackupsClient.Create(ctx, etcdBackup, v1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				backup, err = etcdBackupsClient.Get(ctx, backupName, v1.GetOptions{})
				if err != nil {
					return fmt.Errorf("BackupPolicyController failed to retrieve duplicate backup %s: %w", backupName, err)
				}
				if backup.Labels[backuphelpers.LabelEtcdBackupPolicy] == backupPolicy.Name {
					active = append(active, operatorv1alpha1.EtcdBackupReference{Name: backup.Name, UID: string(backup.UID)})
				}

			} else {
				failedCreations = append(failedCreations, node.Name)
				klog.Warningf("Failed to create EtcdBackup %s for node %s: %v", backupName, node.Name, err)
			}
		} else {
			active = append(active, operatorv1alpha1.EtcdBackupReference{Name: backup.Name, UID: string(backup.UID)})
			klog.V(2).Infof("Created EtcdBackup %s for node %s", backupName, node.Name)
		}
	}

	// Update Backup status with last execution time
	backupPolicy = backupPolicy.DeepCopy()
	backupPolicy.Status.Active = active
	backupPolicy.Status.LastScheduleTime = &v1.Time{Time: time.Now()}
	if _, err := c.operatorClient.EtcdBackupPolicies().UpdateStatus(ctx, backupPolicy, v1.UpdateOptions{}); err != nil {
		// Don't fail the backup execution if status update fails
		klog.Warningf("Failed to update backup status: %v", err)
	}

	if len(failedCreations) > 0 {
		c.eventRecorder.Warningf("PartialBackupFailure",
			"Failed to create backups for nodes: %v", failedCreations)
	} else {
		c.eventRecorder.Eventf("BackupScheduled",
			"Created %d EtcdBackup resources for scheduled backup", len(masterNodes))
	}

	return nil
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

func generateEtcdBackupName(backupPolicyName string, nodeUID types.UID, scheduleTime time.Time) string {
	// Use a "minute hash" to generate backup names from a policy. Schedules can't fire more
	// than once per minute, and this ensures backups aren't duplicated.
	minutesHash := strconv.FormatInt(scheduleTime.Unix()/60, 10)

	uid := strings.ReplaceAll(string(nodeUID), "-", "")
	maxLen := maxNameLength - len(uid) - len(minutesHash) - 2
	if len(backupPolicyName) > maxLen {
		backupPolicyName = backupPolicyName[:maxLen]
	}
	return backupPolicyName + "-" + uid + "-" + minutesHash
}

func nextScheduleTime(backupPolicy *operatorv1alpha1.EtcdBackupPolicy, now time.Time, schedule cron.Schedule) (*time.Time, error) {
	_, mostRecentTime, missedSchedules, err := mostRecentScheduleTime(backupPolicy, now, schedule)

	if mostRecentTime == nil || mostRecentTime.After(now) {
		return nil, err
	}

	if missedSchedules > 100 {
		klog.Warningf("BackupPolicyController missed %d backup start times", missedSchedules)
	}
	return mostRecentTime, err
}

func mostRecentScheduleTime(backupPolicy *operatorv1alpha1.EtcdBackupPolicy, now time.Time, schedule cron.Schedule) (time.Time, *time.Time, int64, error) {
	earliestTime := backupPolicy.CreationTimestamp.Time
	if backupPolicy.Status.LastScheduleTime != nil {
		earliestTime = backupPolicy.Status.LastScheduleTime.Time
	}

	t1 := schedule.Next(earliestTime)
	t2 := schedule.Next(t1)

	if now.Before(t1) {
		return earliestTime, nil, 0, nil
	}
	if now.Before(t2) {
		return earliestTime, &t1, 0, nil
	}

	// It is possible for cron.ParseStandard("59 23 31 2 *") to return an invalid schedule
	// minute - 59, hour - 23, dom - 31, month - 2, and dow is optional, clearly 31 is invalid
	// In this case the timeBetweenTwoSchedules will be 0, and we error out the invalid schedule
	timeBetweenTwoSchedules := int64(t2.Sub(t1).Round(time.Second).Seconds())
	if timeBetweenTwoSchedules < 1 {
		return earliestTime, nil, 0, fmt.Errorf("time difference between two schedules is less than 1 second")
	}
	// this logic used for calculating number of missed schedules does a rough
	// approximation, by calculating a diff between two schedules (t1 and t2),
	// and counting how many of these will fit in between last schedule and now
	timeElapsed := int64(now.Sub(t1).Seconds())
	numberOfMissedSchedules := (timeElapsed / timeBetweenTwoSchedules) + 1

	var mostRecentTime time.Time
	// to get the most recent time accurate for regular schedules and the ones
	// specified with @every form, we first need to calculate the potential earliest
	// time by multiplying the initial number of missed schedules by its interval,
	// this is critical to ensure @every starts at the correct time, this explains
	// the numberOfMissedSchedules-1, the additional -1 serves there to go back
	// in time one more time unit, and let the cron library calculate a proper
	// schedule, for case where the schedule is not consistent, for example
	// something like  30 6-16/4 * * 1-5
	potentialEarliest := t1.Add(time.Duration((numberOfMissedSchedules-1-1)*timeBetweenTwoSchedules) * time.Second)
	for t := schedule.Next(potentialEarliest); !t.After(now); t = schedule.Next(t) {
		mostRecentTime = t
	}

	if mostRecentTime.IsZero() {
		return earliestTime, nil, numberOfMissedSchedules, nil
	}
	return earliestTime, &mostRecentTime, numberOfMissedSchedules, nil
}

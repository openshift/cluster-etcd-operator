package periodicbackupcontroller

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/backuprestore"

	clientv1 "k8s.io/client-go/listers/core/v1"

	backupv1alpha1 "github.com/openshift/api/config/v1alpha1"
	operatorv1 "github.com/openshift/api/operator/v1"
	backupv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1alpha1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	appv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	batchv1client "k8s.io/client-go/kubernetes/typed/batch/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	backupJobLabel                = "backup-name"
	defaultBackupCRName           = "default"
	etcdBackupServerContainerName = "etcd-backup-server"
	backupServerDaemonSet         = "backup-server-daemon-set"
	etcdEndpointConfigMapName     = "etcd-endpoints"
	etcdClientPort                = ":2379"
	etcdDataDirVolName            = "data-dir"
	etcdDataDirVolPath            = "/var/lib/etcd"
	etcdConfigDirVolName          = "config-dir"
	etcdConfigDirVolPath          = "/etc/kubernetes"
	etcdAutoBackupDirVolName      = "etcd-auto-backup-dir"
	etcdAutoBackupDirVolPath      = "/var/lib/etcd-auto-backup"
	etcdCertDirVolName            = "cert-dir"
	etcdCertDirVolPath            = "/etc/kubernetes/static-pod-certs"
	nodeNameEnvVar                = "NODE_NAME"
	nodeNameFieldPath             = "spec.nodeName"
	masterNodeSelector            = "node-role.kubernetes.io/master"
	votingNodeSelector            = "node-role.kubernetes.io/voting"
	backupDSLabelKey              = "app"
	backupDSLabelValue            = "etcd-auto-backup"
)

type PeriodicBackupController struct {
	operatorClient        v1helpers.OperatorClient
	podLister             clientv1.PodLister
	backupsClient         backupv1client.BackupsGetter
	kubeClient            kubernetes.Interface
	operatorImagePullSpec string
	backupVarGetter       backuphelpers.BackupVar
	featureGateAccessor   featuregates.FeatureGateAccess
	kubeInformers         v1helpers.KubeInformersForNamespaces
}

func NewPeriodicBackupController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.OperatorClient,
	backupsClient backupv1client.BackupsGetter,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	operatorImagePullSpec string,
	accessor featuregates.FeatureGateAccess,
	backupVarGetter backuphelpers.BackupVar,
	backupsInformer factory.Informer,
	kubeInformers v1helpers.KubeInformersForNamespaces) factory.Controller {

	c := &PeriodicBackupController{
		operatorClient:        operatorClient,
		podLister:             kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Lister(),
		backupsClient:         backupsClient,
		kubeClient:            kubeClient,
		operatorImagePullSpec: operatorImagePullSpec,
		backupVarGetter:       backupVarGetter,
		featureGateAccessor:   accessor,
		kubeInformers:         kubeInformers,
	}

	syncCtx := factory.NewSyncContext("PeriodicBackupController", eventRecorder.WithComponentSuffix("periodic-backup-controller"))
	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("PeriodicBackupController", syncer)

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(1*time.Minute).
		WithInformers(backupsInformer,
			kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer()).
		WithSync(syncer.Sync).
		ToController("PeriodicBackupController", syncCtx.Recorder())
}

func (c *PeriodicBackupController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	if enabled, err := backuphelpers.AutoBackupFeatureGateEnabled(c.featureGateAccessor); !enabled {
		if err != nil {
			klog.V(4).Infof("PeriodicBackupController error while checking feature flags: %v", err)
		}
		return nil
	}

	cronJobsClient := c.kubeClient.BatchV1().CronJobs(operatorclient.TargetNamespace)
	backups, err := c.backupsClient.Backups().List(ctx, v1.ListOptions{})
	if err != nil {
		return fmt.Errorf("PeriodicBackupController could not list backup CRDs, error was: %w", err)
	}

	defaultFound := false
	for _, item := range backups.Items {
		if item.Name == defaultBackupCRName {
			defaultFound = true

			currentEtcdBackupDS, err := c.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Get(ctx, backupServerDaemonSet, v1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("PeriodicBackupController could not retrieve ds/%s: %w", backupServerDaemonSet, err)
			}

			endpoints, err := getEtcdEndpoints(ctx, c.kubeClient)
			if err != nil {
				return fmt.Errorf("PeriodicBackupController failed to list etcd-endpoints config-map: %w", err)
			}

			if err = ensureVotingNodesLabeled(ctx, c.kubeClient); err != nil {
				return fmt.Errorf("PeriodicBackupController could not label voting master nodes: %w", err)
			}

			desiredEtcdBackupDS := createBackupServerDaemonSet(item, endpoints)
			if etcdBackupServerDSDiffers(desiredEtcdBackupDS.Spec, currentEtcdBackupDS.Spec) {
				_, opStatus, _, oErr := c.operatorClient.GetOperatorState()
				if oErr != nil {
					return fmt.Errorf("PeriodicBackupController could not retrieve operator's state: %w", err)
				}
				_, _, dErr := resourceapply.ApplyDaemonSet(ctx, c.kubeClient.AppsV1(), syncContext.Recorder(), desiredEtcdBackupDS,
					resourcemerge.ExpectedDaemonSetGeneration(desiredEtcdBackupDS, opStatus.Generations),
				)
				if dErr != nil {
					return fmt.Errorf("PeriodicBackupController could not apply ds/%v: %w", backupServerDaemonSet, err)
				}
				klog.V(4).Infof("PeriodicBackupController applied DaemonSet [%v] successfully", backupServerDaemonSet)
			}
			continue
		}

		err := reconcileCronJob(ctx, cronJobsClient, item, c.operatorImagePullSpec)
		if err != nil {
			_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    "PeriodicBackupControllerDegraded",
				Status:  operatorv1.ConditionTrue,
				Reason:  "Error",
				Message: err.Error(),
			}))
			if updateErr != nil {
				klog.V(4).Infof("PeriodicBackupController error during UpdateStatus: %v", err)
			}

			return fmt.Errorf("PeriodicBackupController could not reconcile backup [%s] with cronjob: %w", item.Name, err)
		}
	}

	if !defaultFound {
		err = c.kubeClient.AppsV1().DaemonSets(operatorclient.TargetNamespace).Delete(ctx, backupServerDaemonSet, v1.DeleteOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("PeriodicBackupController could not delete ds/%s: %w", backupServerDaemonSet, err)
			}
			klog.V(4).Infof("PeriodicBackupController deleted DaemonSet [%v] successfully", backupServerDaemonSet)
		}
		err = ensureVotingNodesUnLabeled(ctx, c.kubeClient)
		if err != nil {
			return fmt.Errorf("PeriodicBackupController could not unlabel voting master nodes: %w", err)
		}
	} else {
		terminationReasons, err := checkBackupServerPodsStatus(c.podLister)
		if err != nil {
			return err
		}

		if len(terminationReasons) > 0 {
			_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    "PeriodicBackupControllerDegraded",
				Status:  operatorv1.ConditionTrue,
				Reason:  "Error",
				Message: fmt.Sprintf("found default backup errors: %s", strings.Join(terminationReasons, " ,")),
			}))
			if updateErr != nil {
				klog.V(4).Infof("PeriodicBackupController error during [etcd-backup-server] UpdateStatus: %v", err)
			}
			return nil
		}
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "PeriodicBackupControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}))
	if updateErr != nil {
		klog.V(4).Infof("PeriodicBackupController error during UpdateStatus: %v", err)
	}

	return nil
}

func reconcileCronJob(ctx context.Context,
	cronJobClient batchv1client.CronJobInterface,
	backup backupv1alpha1.Backup,
	operatorImagePullSpec string) error {

	create := false
	currentCronJob, err := cronJobClient.Get(ctx, backup.Name, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			create = true
		} else {
			return fmt.Errorf("PeriodicBackupController could not get cronjob %s: %w", backup.Name, err)
		}
	}

	cronJob, err := newCronJob()
	if err != nil {
		return err
	}

	cronJob.Name = backup.Name
	cronJob.Labels[backupJobLabel] = backup.Name
	cronJob.OwnerReferences = append(cronJob.OwnerReferences, v1.OwnerReference{
		APIVersion: backup.APIVersion,
		Kind:       backup.Kind,
		Name:       backup.Name,
		UID:        backup.UID,
	})

	injected := false
	for _, mount := range cronJob.Spec.JobTemplate.Spec.Template.Spec.Volumes {
		if mount.Name == "etc-kubernetes-cluster-backup" {
			mount.PersistentVolumeClaim.ClaimName = backup.Spec.EtcdBackupSpec.PVCName
			injected = true
			break
		}
	}

	if !injected {
		return fmt.Errorf("could not inject PVC into CronJob template, please check the included cluster-backup-cronjob.yaml")
	}

	cronJob.Spec.Schedule = backup.Spec.EtcdBackupSpec.Schedule
	if backup.Spec.EtcdBackupSpec.TimeZone != "" {
		cronJob.Spec.TimeZone = &backup.Spec.EtcdBackupSpec.TimeZone
	}

	err = setRetentionPolicyInitContainer(backup.Spec.EtcdBackupSpec.RetentionPolicy, cronJob, operatorImagePullSpec)
	if err != nil {
		return err
	}

	if len(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers) == 0 {
		cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers = append(cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers, corev1.Container{})
	}

	cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = operatorImagePullSpec
	// The "etcd-operator request-backup" cmd is passed the pvcName arg it can set that on the EtcdBackup CustomResource spec.
	// The name of the CR will need to be unique for each scheduled run of the CronJob, so the name is
	// set at runtime as the pod via the MY_POD_NAME populated via the downward API.
	// See the CronJob template manifest for reference.
	cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Args = []string{
		"request-backup",
		"--pvc-name=" + backup.Spec.EtcdBackupSpec.PVCName,
	}

	if create {
		_, err := cronJobClient.Create(ctx, cronJob, v1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("PeriodicBackupController could not create cronjob %s: %w", cronJob.Name, err)
		}
	} else {
		if cronSpecDiffers(cronJob.Spec, currentCronJob.Spec) {
			klog.V(4).Infof("detected diff in cronjob specs, updating cronjob: %s", cronJob.Name)
			_, err := cronJobClient.Update(ctx, cronJob, v1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("PeriodicBackupController could not update cronjob %s: %w", cronJob.Name, err)
			}
		}
	}

	return nil
}

func setRetentionPolicyInitContainer(retentionPolicy backupv1alpha1.RetentionPolicy, cronJob *batchv1.CronJob, operatorImagePullSpec string) error {
	retentionType := retentionPolicy.RetentionType
	if retentionPolicy.RetentionType == "" {
		retentionType = backupv1alpha1.RetentionTypeNumber
	}

	retentionInitArgs := []string{
		"prune-backups",
		fmt.Sprintf("--type=%s", retentionType),
	}

	if retentionType == backupv1alpha1.RetentionTypeNumber {
		maxNum := 15
		if retentionPolicy.RetentionNumber != nil {
			maxNum = retentionPolicy.RetentionNumber.MaxNumberOfBackups
		}

		retentionInitArgs = append(retentionInitArgs, fmt.Sprintf("--maxNumberOfBackups=%d", maxNum))
	} else if retentionType == backupv1alpha1.RetentionTypeSize {
		// that's not defined in the API, but we assume that's about 15-20 median sized snapshots in line with MaxNumberOfBackups
		maxSizeGb := 10
		if retentionPolicy.RetentionSize != nil {
			maxSizeGb = retentionPolicy.RetentionSize.MaxSizeOfBackupsGb
		}

		retentionInitArgs = append(retentionInitArgs, fmt.Sprintf("--maxSizeOfBackupsGb=%d", maxSizeGb))
	} else {
		return fmt.Errorf("unknown retention type: %s", retentionPolicy.RetentionType)
	}

	cronJob.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Image = operatorImagePullSpec
	cronJob.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0].Args = retentionInitArgs
	return nil
}

func cronSpecDiffers(l batchv1.CronJobSpec, r batchv1.CronJobSpec) bool {
	lBytes, _ := l.Marshal()
	rBytes, _ := r.Marshal()

	if len(lBytes) != len(rBytes) {
		return true
	}

	for i := 0; i < len(lBytes); i++ {
		if lBytes[i] != rBytes[i] {
			return true
		}
	}

	return false
}

func newCronJob() (*batchv1.CronJob, error) {
	scheme := runtime.NewScheme()
	codec := serializer.NewCodecFactory(scheme)
	err := batchv1.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("PeriodicBackupController could not add batchv1 scheme: %w", err)
	}

	obj, err := runtime.Decode(codec.UniversalDecoder(batchv1.SchemeGroupVersion), etcd_assets.MustAsset("etcd/cluster-backup-cronjob.yaml"))
	if err != nil {
		return nil, fmt.Errorf("PeriodicBackupController could not decode batchv1 job scheme: %w", err)
	}

	return obj.(*batchv1.CronJob), nil
}

func createBackupServerDaemonSet(cr backupv1alpha1.Backup, endpoints []string) *appv1.DaemonSet {
	ds := appv1.DaemonSet{
		ObjectMeta: v1.ObjectMeta{
			Name:      backupServerDaemonSet,
			Namespace: operatorclient.TargetNamespace,
			Labels: map[string]string{
				backupDSLabelKey: backupDSLabelValue,
			},
		},
		Spec: appv1.DaemonSetSpec{
			Selector: &v1.LabelSelector{
				MatchLabels: map[string]string{
					backupDSLabelKey: backupDSLabelValue,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Labels: map[string]string{
						backupDSLabelKey: backupDSLabelValue,
					},
				},
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{
							Key:    masterNodeSelector,
							Effect: corev1.TaintEffectNoSchedule,
						},
						{
							Key:    votingNodeSelector,
							Effect: corev1.TaintEffectNoSchedule,
						},
					},
					NodeSelector: map[string]string{
						masterNodeSelector: "",
						votingNodeSelector: "",
					},
					Volumes: []corev1.Volume{
						{Name: etcdDataDirVolName, VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: etcdDataDirVolPath,
								Type: ptr.To(corev1.HostPathUnset)}}},

						{Name: etcdConfigDirVolName, VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: etcdConfigDirVolPath}}},

						{Name: etcdAutoBackupDirVolName, VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: etcdAutoBackupDirVolPath}}},

						{Name: etcdCertDirVolName, VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/etc/kubernetes/static-pod-resources/etcd-certs"}}},
					},
					Containers: []corev1.Container{
						{
							Name:    etcdBackupServerContainerName,
							Image:   os.Getenv("OPERATOR_IMAGE"),
							Command: []string{"cluster-etcd-operator", "backup-server"},
							Env: []corev1.EnvVar{
								{
									Name: nodeNameEnvVar,
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: nodeNameFieldPath,
										},
									},
								},
							},
							Args: constructBackupServerArgs(cr, strings.Join(endpoints, ",")),
							VolumeMounts: []corev1.VolumeMount{
								{Name: etcdDataDirVolName, MountPath: etcdDataDirVolPath},
								{Name: etcdConfigDirVolName, MountPath: etcdConfigDirVolPath},
								{Name: etcdAutoBackupDirVolName, MountPath: etcdAutoBackupDirVolPath},
								{Name: etcdCertDirVolName, MountPath: etcdCertDirVolPath},
							},
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
							ImagePullPolicy:          corev1.PullIfNotPresent,
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
						},
					},
				},
			},
		},
	}

	return &ds
}

func getEtcdEndpoints(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	etcdEndPointsCM, err := client.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(ctx, etcdEndpointConfigMapName, v1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var endpoints []string
	for _, v := range etcdEndPointsCM.Data {
		ep := v + etcdClientPort
		endpoints = append(endpoints, ep)
	}

	slices.Sort(endpoints)
	return endpoints, nil
}

func constructBackupServerArgs(cr backupv1alpha1.Backup, endpoints string) []string {
	backupConfig := backuphelpers.NewDisabledBackupConfig()
	backupConfig.SetBackupSpec(&cr.Spec.EtcdBackupSpec)
	argList := backupConfig.ArgList()

	argList = append(argList, fmt.Sprintf("--endpoints=%s", endpoints))
	argList = append(argList, fmt.Sprintf("--backupPath=%s", backuprestore.BackupVolume))

	return argList
}

func checkBackupServerPodsStatus(podLister clientv1.PodLister) ([]string, error) {
	backupPods, err := podLister.List(labels.Set{backupDSLabelKey: backupDSLabelValue}.AsSelector())
	if err != nil {
		return nil, fmt.Errorf("PeriodicBackupController could not list etcd pods: %w", err)
	}

	var terminationReasons []string
	for _, p := range backupPods {
		if p.Status.Phase == corev1.PodFailed {
			for _, cStatus := range p.Status.ContainerStatuses {
				if cStatus.Name == etcdBackupServerContainerName {
					// TODO we can also try different cStatus.State.Terminated.ExitCode
					terminationReasons = append(terminationReasons, fmt.Sprintf("container %s within pod %s has been terminated: %s", etcdBackupServerContainerName, p.Name, cStatus.State.Terminated.Message))
				}
			}
		}
	}

	return terminationReasons, nil
}

func etcdBackupServerDSDiffers(l, r appv1.DaemonSetSpec) bool {
	lBytes, _ := l.Marshal()
	rBytes, _ := r.Marshal()

	if len(lBytes) != len(rBytes) {
		return true
	}

	for i := 0; i < len(lBytes); i++ {
		if lBytes[i] != rBytes[i] {
			return true
		}
	}

	return false
}

func ensureVotingNodesLabeled(ctx context.Context, client kubernetes.Interface) error {
	etcdEndPointsCM, err := client.CoreV1().ConfigMaps(operatorclient.TargetNamespace).Get(ctx, etcdEndpointConfigMapName, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to retrieve etcd-endpoints configmap: %v", err)
	}

	votingIPs := sets.NewString()
	for _, v := range etcdEndPointsCM.Data {
		votingIPs.Insert(v)
	}

	masterNodes, err := client.CoreV1().Nodes().List(ctx, v1.ListOptions{
		LabelSelector: labels.Set{masterNodeSelector: ""}.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list master nodes: %w", err)
	}

	for _, node := range masterNodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				if votingIPs.Has(addr.Address) {
					// update node's labels
					node.Labels[votingNodeSelector] = ""
					_, err = client.CoreV1().Nodes().Update(ctx, &node, v1.UpdateOptions{})
					if err != nil {
						return fmt.Errorf("failed to update master node [%v] with label [%v]", node, votingNodeSelector)
					}
				}
			}
		}
	}

	return nil
}

func ensureVotingNodesUnLabeled(ctx context.Context, client kubernetes.Interface) error {
	masterNodes, err := client.CoreV1().Nodes().List(ctx, v1.ListOptions{
		LabelSelector: labels.Set{masterNodeSelector: ""}.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list master nodes: %w", err)
	}

	for _, node := range masterNodes.Items {
		delete(node.Labels, votingNodeSelector)

		_, err = client.CoreV1().Nodes().Update(ctx, &node, v1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update master node [%v] with deleting label [%v]", node, votingNodeSelector)
		}
	}

	return nil
}

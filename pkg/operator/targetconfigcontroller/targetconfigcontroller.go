package targetconfigcontroller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"text/template"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
)

type NameValue struct {
	Name  string
	Value string
}

type PodSubstitutionTemplate struct {
	Image            string
	OperatorImage    string
	ListenAddress    string
	LocalhostAddress string
	LogLevel         string
	EnvVars          []NameValue
	BackupArgs       []string
}

type TargetConfigController struct {
	targetImagePullSpec   string
	operatorImagePullSpec string

	operatorClient v1helpers.StaticPodOperatorClient

	kubeClient      kubernetes.Interface
	envVarGetter    etcdenvvar.EnvVar
	backupVarGetter backuphelpers.BackupVar

	enqueueFn func()
}

func NewTargetConfigController(
	livenessChecker *health.MultiAlivenessChecker,
	targetImagePullSpec, operatorImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	masterNodeInformer cache.SharedIndexInformer,
	kubeClient kubernetes.Interface,
	envVarGetter etcdenvvar.EnvVar,
	backupVarGetter backuphelpers.BackupVar,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &TargetConfigController{
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,

		operatorClient:  operatorClient,
		kubeClient:      kubeClient,
		envVarGetter:    envVarGetter,
		backupVarGetter: backupVarGetter,
	}

	syncCtx := factory.NewSyncContext("TargetConfigController", eventRecorder.WithComponentSuffix("target-config-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}
	envVarGetter.AddListener(c)
	backupVarGetter.AddListener(c)

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("TargetConfigController", syncer)

	return factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(time.Minute).
		WithSync(syncer.Sync).
		WithInformers(
			operatorClient.Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer(),
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer(),
			kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer(),
			masterNodeInformer,
			infrastructureInformer.Informer(),
			networkInformer.Informer(),
		).ToController("TargetConfigController", syncCtx.Recorder())
}

func (c *TargetConfigController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	envVars := c.envVarGetter.GetEnvVars()
	if len(envVars) == 0 {
		// note this will not degrade the controller, that can happen during CEO restarts often due to cold informer caches (expected)
		return fmt.Errorf("TargetConfigController missing env var values")
	}

	operatorSpec, _, _, err := c.operatorClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	err = c.createTargetConfig(ctx, syncCtx.Recorder(), operatorSpec, envVars, c.backupVarGetter)
	if err != nil {
		condition := operatorv1.OperatorCondition{
			Type:    "TargetConfigControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "SynchronizationError",
			Message: err.Error(),
		}
		if _, _, err := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
			// this re-queues the sync loop to get the status updated correctly next invocation
			c.enqueueFn()
			return err
		}

		return err
	}

	condition := operatorv1.OperatorCondition{
		Type:   "TargetConfigControllerDegraded",
		Status: operatorv1.ConditionFalse,
	}
	if _, _, err := v1helpers.UpdateStaticPodStatus(ctx, c.operatorClient, v1helpers.UpdateStaticPodConditionFn(condition)); err != nil {
		// this re-queues the sync loop to get the status updated correctly next invocation
		c.enqueueFn()
		return err
	}

	return nil
}

// createTargetConfig takes care of creation of valid resources in a fixed name. These are inputs to other control loops.
func (c *TargetConfigController) createTargetConfig(
	ctx context.Context,
	recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec,
	envVars map[string]string,
	backupVar backuphelpers.BackupVar) error {

	var errs error
	contentReplacer, err := c.getSubstitutionReplacer(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec, envVars, backupVar)
	if err != nil {
		return err
	}
	podSub := c.getPodSubstitution(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec, envVars, backupVar)
	_, _, err = c.manageStandardPod(ctx, podSub, c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errs = errors.Join(errs, fmt.Errorf("%q: %w", "configmap/etcd-pod", err))
	}

	_, _, err = c.manageRecoveryPods(ctx, contentReplacer, c.kubeClient.CoreV1(), recorder, operatorSpec)
	if err != nil {
		errs = errors.Join(errs, fmt.Errorf("%q: %w", "configmap/restore-etcd-pod", err))
	}

	return errs
}

func loglevelToZap(logLevel operatorv1.LogLevel) string {
	switch logLevel {
	case operatorv1.Debug, operatorv1.Trace, operatorv1.TraceAll:
		return "debug"
	default:
		return "info"
	}
}

func (c *TargetConfigController) getSubstitutionReplacer(operatorSpec *operatorv1.StaticPodOperatorSpec,
	imagePullSpec, operatorImagePullSpec string, envVarMap map[string]string, backupVar backuphelpers.BackupVar) (*strings.Replacer, error) {
	var envVarLines []string
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("      - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("        value: %q", v))
	}

	return strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${OPERATOR_IMAGE}", operatorImagePullSpec,
		"${VERBOSITY}", loglevelToZap(operatorSpec.LogLevel),
		"${LISTEN_ON_ALL_IPS}", "0.0.0.0", // TODO this needs updating to detect ipv6-ness
		"${LOCALHOST_IP}", "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		"${COMPUTED_ENV_VARS}", strings.Join(envVarLines, "\n"), // lacks beauty, but it works
		"${COMPUTED_BACKUP_VARS}", backupVar.ArgString(),
	), nil
}

func (c *TargetConfigController) getPodSubstitution(operatorSpec *operatorv1.StaticPodOperatorSpec,
	imagePullSpec, operatorImagePullSpec string, envVarMap map[string]string, backupVar backuphelpers.BackupVar) *PodSubstitutionTemplate {

	var nameValues []NameValue
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		nameValues = append(nameValues, NameValue{k, v})
	}

	return &PodSubstitutionTemplate{
		Image:            imagePullSpec,
		OperatorImage:    operatorImagePullSpec,
		ListenAddress:    "0.0.0.0",   // TODO this needs updating to detect ipv6-ness
		LocalhostAddress: "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		LogLevel:         loglevelToZap(operatorSpec.LogLevel),
		EnvVars:          nameValues,
		BackupArgs:       backupVar.ArgList(),
	}
}

func (c *TargetConfigController) manageRecoveryPods(
	ctx context.Context,
	substitutionReplacer *strings.Replacer,
	client coreclientv1.ConfigMapsGetter,
	recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/restore-pod-cm.yaml"))
	restorePodBytes := etcd_assets.MustAsset("etcd/restore-pod.yaml")
	podConfigMap.Data["pod.yaml"] = substitutionReplacer.Replace(string(restorePodBytes))
	quorumRestorePodBytes := etcd_assets.MustAsset("etcd/quorum-restore-pod.yaml")
	podConfigMap.Data["quorum-restore-pod.yaml"] = substitutionReplacer.Replace(string(quorumRestorePodBytes))
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *TargetConfigController) manageStandardPod(ctx context.Context, subs *PodSubstitutionTemplate,
	client coreclientv1.ConfigMapsGetter, recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {

	fm := template.FuncMap{"quote": func(arg reflect.Value) string {
		return "\"" + arg.String() + "\""
	}}
	podBytes := etcd_assets.MustAsset("etcd/pod.gotpl.yaml")
	tmpl, err := template.New("pod").Funcs(fm).Parse(string(podBytes))
	if err != nil {
		return nil, false, err
	}

	w := &strings.Builder{}
	err = tmpl.Execute(w, subs)
	if err != nil {
		return nil, false, err
	}

	podConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = w.String()
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *TargetConfigController) Enqueue() {
	c.enqueueFn()
}

func (c *TargetConfigController) namespaceEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				c.Enqueue()
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
		UpdateFunc: func(old, new interface{}) {
			ns, ok := old.(*corev1.Namespace)
			if !ok {
				c.Enqueue()
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
		DeleteFunc: func(obj interface{}) {
			ns, ok := obj.(*corev1.Namespace)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
					return
				}
				ns, ok = tombstone.Obj.(*corev1.Namespace)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace %#v", obj))
					return
				}
			}
			if ns.Name == ("openshift-etcd") {
				c.Enqueue()
			}
		},
	}
}

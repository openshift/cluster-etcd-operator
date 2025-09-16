package targetconfigcontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	operatorv1listers "github.com/openshift/client-go/operator/listers/operator/v1"
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

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/status"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
)

type TargetConfigController struct {
	targetImagePullSpec   string
	operatorImagePullSpec string

	operatorClient           v1helpers.StaticPodOperatorClient
	dualReplicaClusterStatus status.ClusterStatus

	kubeClient   kubernetes.Interface
	envVarGetter etcdenvvar.EnvVar
	etcdLister   operatorv1listers.EtcdLister

	enqueueFn func()
}

func NewTargetConfigController(
	livenessChecker *health.MultiAlivenessChecker,
	targetImagePullSpec, operatorImagePullSpec string,
	operatorClient v1helpers.StaticPodOperatorClient,
	dualReplicaClusterStatus status.ClusterStatus,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	masterNodeInformer cache.SharedIndexInformer,
	etcdsInformer operatorv1informers.EtcdInformer,
	kubeClient kubernetes.Interface,
	envVarGetter etcdenvvar.EnvVar,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &TargetConfigController{
		targetImagePullSpec:   targetImagePullSpec,
		operatorImagePullSpec: operatorImagePullSpec,

		operatorClient:           operatorClient,
		dualReplicaClusterStatus: dualReplicaClusterStatus,
		kubeClient:               kubeClient,
		envVarGetter:             envVarGetter,
		etcdLister:               etcdsInformer.Lister(),
	}

	syncCtx := factory.NewSyncContext("TargetConfigController", eventRecorder.WithComponentSuffix("target-config-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}
	envVarGetter.AddListener(c)

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
			etcdsInformer.Informer(),
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

	etcd, err := c.etcdLister.Get("cluster")
	if err != nil {
		return err
	}

	// Check status of dual replica cluster aka Two Node Fencing
	shouldRemoveEtcdContainer := false
	if c.dualReplicaClusterStatus.IsDualReplicaTopology() && c.dualReplicaClusterStatus.IsReadyForEtcdRemoval() {
		shouldRemoveEtcdContainer = c.dualReplicaClusterStatus.IsReadyForEtcdRemoval()
	}

	err = c.createTargetConfig(ctx, syncCtx.Recorder(), operatorSpec, envVars, etcd, shouldRemoveEtcdContainer)
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
	etcd *operatorv1.Etcd,
	shouldRemoveEtcdContainer bool) error {

	var errs error
	contentReplacer, err := c.getSubstitutionReplacer(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec, envVars)
	if err != nil {
		return err
	}

	podSub, err := ceohelpers.GetPodSubstitution(operatorSpec, c.targetImagePullSpec, c.operatorImagePullSpec, envVars, etcd, shouldRemoveEtcdContainer)
	if err != nil {
		return err
	}

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

func (c *TargetConfigController) getSubstitutionReplacer(operatorSpec *operatorv1.StaticPodOperatorSpec,
	imagePullSpec, operatorImagePullSpec string, envVarMap map[string]string) (*strings.Replacer, error) {
	var envVarLines []string
	for _, k := range sets.StringKeySet(envVarMap).List() {
		v := envVarMap[k]
		envVarLines = append(envVarLines, fmt.Sprintf("      - name: %q", k))
		envVarLines = append(envVarLines, fmt.Sprintf("        value: %q", v))
	}

	return strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${OPERATOR_IMAGE}", operatorImagePullSpec,
		"${VERBOSITY}", ceohelpers.LoglevelToZap(operatorSpec.LogLevel),
		"${LISTEN_ON_ALL_IPS}", "0.0.0.0", // TODO this needs updating to detect ipv6-ness
		"${LOCALHOST_IP}", "127.0.0.1", // TODO this needs updating to detect ipv6-ness
		"${COMPUTED_ENV_VARS}", strings.Join(envVarLines, "\n"), // lacks beauty, but it works
	), nil
}

func (c *TargetConfigController) manageRecoveryPods(
	ctx context.Context,
	substitutionReplacer *strings.Replacer,
	client coreclientv1.ConfigMapsGetter,
	recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {
	podConfigMap := resourceread.ReadConfigMapV1OrDie(bindata.MustAsset("etcd/restore-pod-cm.yaml"))
	restorePodBytes := bindata.MustAsset("etcd/restore-pod.yaml")
	podConfigMap.Data["pod.yaml"] = substitutionReplacer.Replace(string(restorePodBytes))
	quorumRestorePodBytes := bindata.MustAsset("etcd/quorum-restore-pod.yaml")
	podConfigMap.Data["quorum-restore-pod.yaml"] = substitutionReplacer.Replace(string(quorumRestorePodBytes))
	podConfigMap.Data["forceRedeploymentReason"] = operatorSpec.ForceRedeploymentReason
	podConfigMap.Data["version"] = version.Get().String()
	return resourceapply.ApplyConfigMap(ctx, client, recorder, podConfigMap)
}

func (c *TargetConfigController) manageStandardPod(ctx context.Context, subs *ceohelpers.PodSubstitutionTemplate,
	client coreclientv1.ConfigMapsGetter, recorder events.Recorder,
	operatorSpec *operatorv1.StaticPodOperatorSpec) (*corev1.ConfigMap, bool, error) {

	renderedTemplate, err := ceohelpers.RenderTemplate("etcd/pod.gotpl.yaml", subs)
	if err != nil {
		return nil, false, err
	}

	podConfigMap := resourceread.ReadConfigMapV1OrDie(bindata.MustAsset("etcd/pod-cm.yaml"))
	podConfigMap.Data["pod.yaml"] = renderedTemplate
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

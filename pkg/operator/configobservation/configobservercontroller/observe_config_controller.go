package configobservercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/imdario/mergo"

	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/controlplanereplicascount"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	libgoapiserver "github.com/openshift/library-go/pkg/operator/configobserver/apiserver"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	// configObserverFieldManager is the SSA field manager name used when applying
	// observedConfig to the Etcd operator resource. Using a dedicated field manager
	// ensures that the config observer's writes to .spec.observedConfig do not
	// conflict with other controllers that update .status or other .spec fields.
	configObserverFieldManager = "cluster-etcd-operator-config-observer"
)

type ConfigObserver struct {
	factory.Controller
}

func NewConfigObserver(
	operatorClient v1helpers.OperatorClient,
	configInformer configinformers.SharedInformerFactory,
	operatorConfigInformers operatorv1informers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	masterNodeInformer cache.SharedIndexInformer,
	masterNodeLister corev1listers.NodeLister,
	resourceSyncer resourcesynccontroller.ResourceSyncer,
	eventRecorder events.Recorder,
) *ConfigObserver {
	interestingNamespaces := []string{
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		"openshift-etcd",
		operatorclient.OperatorNamespace,
		"kube-system",
	}

	configMapPreRunCacheSynced := []cache.InformerSynced{}
	for _, ns := range interestingNamespaces {
		configMapPreRunCacheSynced = append(configMapPreRunCacheSynced, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer().HasSynced)
	}

	informers := []factory.Informer{
		operatorConfigInformers.Operator().V1().Etcds().Informer(),
		configInformer.Config().V1().APIServers().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer(),
		kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer(),
		masterNodeInformer,
	}

	for _, ns := range interestingNamespaces {
		informers = append(informers, kubeInformersForNamespaces.InformersFor(ns).Core().V1().ConfigMaps().Informer())
	}

	listers := configobservation.Listers{
		APIServerLister_: configInformer.Config().V1().APIServers().Lister(),

		OpenshiftEtcdEndpointsLister:          kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Lister(),
		OpenshiftEtcdPodsLister:               kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Lister(),
		OpenshiftEtcdConfigMapsLister:         kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Lister(),
		NodeLister:                            masterNodeLister,
		ConfigMapListerForKubeSystemNamespace: kubeInformersForNamespaces.InformersFor("kube-system").Core().V1().ConfigMaps().Lister().ConfigMaps("kube-system"),

		ResourceSync: resourceSyncer,
		PreRunCachesSynced: append(configMapPreRunCacheSynced,
			operatorClient.Informer().HasSynced,
			operatorConfigInformers.Operator().V1().Etcds().Informer().HasSynced,
			configInformer.Config().V1().APIServers().Informer().HasSynced,

			kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Endpoints().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().Pods().Informer().HasSynced,
			kubeInformersForNamespaces.InformersFor("openshift-etcd").Core().V1().ConfigMaps().Informer().HasSynced,
			masterNodeInformer.HasSynced,
		),
	}

	observers := []configobserver.ObserveConfigFunc{
		libgoapiserver.ObserveTLSSecurityProfile,
		controlplanereplicascount.ObserveControlPlaneReplicas,
	}

	ssaObserver := &ssaConfigObserver{
		controllerInstanceName: factory.ControllerInstanceName("etcd", "ConfigObserver"),
		operatorClient:         operatorClient,
		observers:              observers,
		listers:                listers,
		degradedConditionType:  condition.ConfigObservationDegradedConditionType,
	}

	controller := factory.New().
		ResyncEvery(time.Minute).
		WithSync(ssaObserver.sync).
		WithControllerInstanceName(ssaObserver.controllerInstanceName).
		WithInformers(append(informers, listersToInformer(listers)...)...).
		ToController(
			"ConfigObserver", // don't change what is passed here unless you also remove the old FooDegraded condition
			eventRecorder.WithComponentSuffix("config-observer"),
		)

	c := &ConfigObserver{
		Controller: controller,
	}

	return c
}

// ssaConfigObserver implements the config observer sync loop using Server-Side Apply
// to write the observedConfig. This prevents 409 Conflict errors during bootstrap
// when multiple controllers are rapidly updating the same Etcd CR's status, which
// causes resourceVersion churn that blocks the traditional read-modify-write approach
// used by the library-go ConfigObserver.
type ssaConfigObserver struct {
	controllerInstanceName string
	operatorClient         v1helpers.OperatorClient
	observers              []configobserver.ObserveConfigFunc
	listers                configobservation.Listers
	degradedConditionType  string
}

// sync reacts to a change in prereqs by finding information that is required to match
// another value in the cluster. It uses Server-Side Apply to write the observedConfig,
// which avoids the resourceVersion conflicts that occur with the standard read-modify-write
// pattern during bootstrap when many controllers are updating the same CR.
func (c *ssaConfigObserver) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	spec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	// don't worry about errors. If we can't decode, we'll simply stomp over the field.
	existingConfig := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(spec.ObservedConfig.Raw)).Decode(&existingConfig); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}

	var errs []error
	var observedConfigs []map[string]interface{}
	for _, i := range rand.Perm(len(c.observers)) {
		var currErrs []error
		observedConfig, currErrs := c.observers[i](c.listers, syncCtx.Recorder(), existingConfig)
		observedConfigs = append(observedConfigs, observedConfig)
		errs = append(errs, currErrs...)
	}

	mergedObservedConfig := map[string]interface{}{}
	for _, observedConfig := range observedConfigs {
		if err := mergo.Merge(&mergedObservedConfig, observedConfig); err != nil {
			klog.Warningf("merging observed config failed: %v", err)
		}
	}

	reverseMergedObservedConfig := map[string]interface{}{}
	for i := len(observedConfigs) - 1; i >= 0; i-- {
		if err := mergo.Merge(&reverseMergedObservedConfig, observedConfigs[i]); err != nil {
			klog.Warningf("merging observed config failed: %v", err)
		}
	}

	if !equality.Semantic.DeepEqual(mergedObservedConfig, reverseMergedObservedConfig) {
		errs = append(errs, errors.New("non-deterministic config observation detected"))
	}

	if err := c.applyObservedConfig(ctx, syncCtx, existingConfig, mergedObservedConfig); err != nil {
		errs = []error{err}
	}
	configError := v1helpers.NewMultiLineAggregate(errs)

	// update failing condition
	conditionApply := applyoperatorv1.OperatorCondition().
		WithType(c.degradedConditionType).
		WithStatus(operatorv1.ConditionFalse)
	if configError != nil {
		conditionApply = conditionApply.
			WithStatus(operatorv1.ConditionTrue).
			WithReason("Error").
			WithMessage(configError.Error())
	}
	statusApply := applyoperatorv1.OperatorStatus().WithConditions(conditionApply)
	updateError := c.operatorClient.ApplyOperatorStatus(ctx, c.controllerInstanceName, statusApply)
	if updateError != nil {
		return updateError
	}

	return configError
}

// applyObservedConfig writes the merged observed config to the operator CR using
// Server-Side Apply (SSA). Unlike the library-go ConfigObserver which uses
// UpdateSpec (read-modify-write with optimistic concurrency), SSA handles
// field-level ownership merging on the API server side, eliminating 409 Conflict
// errors caused by concurrent status updates from other controllers.
func (c *ssaConfigObserver) applyObservedConfig(ctx context.Context, syncCtx factory.SyncContext, existingConfig, mergedObservedConfig map[string]interface{}) error {
	if equality.Semantic.DeepEqual(existingConfig, mergedObservedConfig) {
		return nil
	}

	syncCtx.Recorder().Eventf("ObservedConfigChanged", "Writing updated observed config via SSA: %v", diff.Diff(existingConfig, mergedObservedConfig))

	configBytes, err := json.Marshal(mergedObservedConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal observed config: %v", err)
	}

	specApply := applyoperatorv1.OperatorSpec().
		WithObservedConfig(runtime.RawExtension{Raw: configBytes})
	if err := c.operatorClient.ApplyOperatorSpec(ctx, configObserverFieldManager, specApply); err != nil {
		syncCtx.Recorder().Warningf("ObservedConfigWriteError", "Failed to write observed config via SSA: %v", err)
		return fmt.Errorf("error writing updated observed config via SSA: %v", err)
	}
	return nil
}

// listersToInformer converts the Listers interface to informer with empty AddEventHandler
// as we only care about synced caches in the Run.
func listersToInformer(l configobservation.Listers) []factory.Informer {
	result := make([]factory.Informer, len(l.PreRunHasSynced()))
	for i := range l.PreRunHasSynced() {
		result[i] = &listerInformer{cacheSynced: l.PreRunHasSynced()[i]}
	}
	return result
}

type listerInformer struct {
	cacheSynced cache.InformerSynced
}

func (l *listerInformer) AddEventHandler(cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	return nil, nil
}

func (l *listerInformer) HasSynced() bool {
	return l.cacheSynced()
}

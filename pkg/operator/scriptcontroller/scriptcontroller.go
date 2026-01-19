package scriptcontroller

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type ScriptController struct {
	operatorClient  v1helpers.StaticPodOperatorClient
	kubeClient      kubernetes.Interface
	configMapLister corev1listers.ConfigMapLister
	envVarGetter    *etcdenvvar.EnvVarController
	enqueueFn       func()
}

func NewScriptControllerController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	envVarGetter *etcdenvvar.EnvVarController,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &ScriptController{
		operatorClient:  operatorClient,
		kubeClient:      kubeClient,
		configMapLister: kubeInformersForNamespaces.ConfigMapLister(),
		envVarGetter:    envVarGetter,
	}
	envVarGetter.AddListener(c)

	syncCtx := factory.NewSyncContext("ScriptController", eventRecorder.WithComponentSuffix("script-controller"))
	c.enqueueFn = func() {
		syncCtx.Queue().Add(syncCtx.QueueKey())
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("ScriptController", syncer)

	return factory.New().WithSyncContext(syncCtx).ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer(),
	).WithSync(syncer.Sync).ToController("ScriptController", syncCtx.Recorder())
}

func (c *ScriptController) Enqueue() {
	c.enqueueFn()
}

func (c ScriptController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.createScriptConfigMap(ctx, syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ScriptControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("ScriptControllerErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "ScriptControllerDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "AsExpected",
	}))
	if updateErr != nil {
		syncCtx.Recorder().Warning("ScriptControllerErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}

	return nil
}

// createScriptController takes care of creation of valid resources in a fixed name.  These are inputs to other control loops.
// returns whether or not requeue and if an error happened when updating status.  Normally it updates status itself.
func (c *ScriptController) createScriptConfigMap(ctx context.Context, recorder events.Recorder) error {
	errors := []error{}

	_, _, err := c.manageScriptConfigMap(ctx, recorder)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap/etcd-pod", err))
	}

	return utilerrors.NewAggregate(errors)
}

func (c *ScriptController) manageScriptConfigMap(ctx context.Context, recorder events.Recorder) (*corev1.ConfigMap, bool, error) {
	scriptConfigMap := resourceread.ReadConfigMapV1OrDie(etcd_assets.MustAsset("etcd/scripts-cm.yaml"))
	// TODO get the env vars to produce a file that we write
	envVarMap := c.envVarGetter.GetEnvVars()
	if len(envVarMap) == 0 {
		return nil, false, fmt.Errorf("missing env var values")
	}
	envVarFileContent := ""
	for _, k := range sets.StringKeySet(envVarMap).List() { // sort for stable output
		envVarFileContent += fmt.Sprintf("export %v=%q\n", k, envVarMap[k])
	}
	scriptConfigMap.Data["etcd.env"] = envVarFileContent

	for _, filename := range []string{
		"etcd/cluster-restore.sh",
		"etcd/cluster-backup.sh",
		"etcd/etcd-common-tools",
	} {
		basename := filepath.Base(filename)
		scriptConfigMap.Data[basename] = string(etcd_assets.MustAsset(filename))
	}
	return resourceapply.ApplyConfigMap(ctx, c.kubeClient.CoreV1(), recorder, scriptConfigMap)
}

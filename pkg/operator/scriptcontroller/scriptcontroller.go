package scriptcontroller

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
)

// EnvVarGetter is an interface for getting environment variables and managing listeners
type EnvVarGetter interface {
	GetEnvVars() map[string]string
	AddListener(listener etcdenvvar.Enqueueable)
}

type ScriptController struct {
	operatorClient v1helpers.StaticPodOperatorClient
	kubeClient     kubernetes.Interface
	infraLister    configv1listers.InfrastructureLister
	envVarGetter   EnvVarGetter
	enqueueFn      func()
}

func NewScriptControllerController(
	livenessChecker *health.MultiAlivenessChecker,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	infraLister configv1listers.InfrastructureLister,
	envVarGetter EnvVarGetter,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &ScriptController{
		operatorClient: operatorClient,
		kubeClient:     kubeClient,
		infraLister:    infraLister,
		envVarGetter:   envVarGetter,
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
	scriptConfigMap := resourceread.ReadConfigMapV1OrDie(bindata.MustAsset("etcd/scripts-cm.yaml"))
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

	// Select the appropriate cluster-restore.sh and disable-etcd.sh based on topology
	var clusterRestoreScript string
	var disableEtcdScript string
	if isTNF, err := ceohelpers.IsExternalEtcdCluster(ctx, c.infraLister); err != nil {
		return nil, false, fmt.Errorf("failed to detect cluster topology: %v", err)
	} else if isTNF {
		clusterRestoreScript = "etcd/cluster-restore-tnf.sh"
		disableEtcdScript = "etcd/disable-etcd-tnf.sh"
	} else {
		clusterRestoreScript = "etcd/cluster-restore.sh"
		disableEtcdScript = "etcd/disable-etcd.sh"
	}

	// Deploy common scripts (same for both TNF and standard)
	for _, filename := range []string{
		"etcd/quorum-restore.sh",
		"etcd/cluster-backup.sh",
		"etcd/etcd-common-tools",
	} {
		basename := filepath.Base(filename)
		scriptConfigMap.Data[basename] = string(bindata.MustAsset(filename))
	}

	// Deploy topology-specific scripts with standard names
	scriptConfigMap.Data["cluster-restore.sh"] = string(bindata.MustAsset(clusterRestoreScript))
	scriptConfigMap.Data["disable-etcd.sh"] = string(bindata.MustAsset(disableEtcdScript))

	return resourceapply.ApplyConfigMap(ctx, c.kubeClient.CoreV1(), recorder, scriptConfigMap)
}

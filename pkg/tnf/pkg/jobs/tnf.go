package jobs

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

var (
	// runningControllers tracks which controllers are already running to prevent duplicates
	runningControllers = make(map[string]bool)
	// runningControllersMutex protects the runningControllers map
	runningControllersMutex sync.Mutex

	// restartJobLocks tracks in-flight RestartJobOrRunController calls to prevent parallel execution
	restartJobLocks = make(map[string]*sync.Mutex)
	// restartJobLocksMutex protects the restartJobLocks map
	restartJobLocksMutex sync.Mutex
)

func RunTNFJobController(ctx context.Context, jobType tools.JobType, nodeName *string, controllerContext *controllercmd.ControllerContext, operatorClient v1helpers.StaticPodOperatorClient, kubeClient kubernetes.Interface, kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces, conditions []string) {
	nodeNameForLogs := "undefined"
	if nodeName != nil {
		nodeNameForLogs = *nodeName
	}

	// Check if a controller for this jobType and nodeName is already running
	controllerKey := jobType.GetJobName(nodeName)
	runningControllersMutex.Lock()
	if runningControllers[controllerKey] {
		runningControllersMutex.Unlock()
		klog.Infof("Two Node Fencing job controller for command %q on node %q is already running, skipping duplicate start", jobType.GetSubCommand(), nodeNameForLogs)
		return
	}
	// Mark this controller as running
	runningControllers[controllerKey] = true
	runningControllersMutex.Unlock()

	klog.Infof("starting Two Node Fencing job controller for command %q on node %q", jobType.GetSubCommand(), nodeNameForLogs)
	tnfJobController := NewJobController(
		jobType.GetJobName(nodeName),
		bindata.MustAsset("tnfdeployment/job.yaml"),
		controllerContext.EventRecorder,
		operatorClient,
		kubeClient,
		kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs(),
		conditions,
		[]factory.Informer{},
		[]JobHookFunc{
			func(_ *operatorv1.OperatorSpec, job *batchv1.Job) error {
				if nodeName != nil {
					job.Spec.Template.Spec.NodeName = *nodeName
				}
				job.SetName(jobType.GetJobName(nodeName))
				job.Labels["app.kubernetes.io/name"] = jobType.GetNameLabelValue()
				job.Spec.Template.Spec.Containers[0].Image = os.Getenv("OPERATOR_IMAGE")
				job.Spec.Template.Spec.Containers[0].Command[1] = jobType.GetSubCommand()
				return nil
			}}...,
	)
	go tnfJobController.Run(ctx, 1)
}

func RestartJobOrRunController(
	ctx context.Context,
	jobType tools.JobType,
	nodeName *string,
	controllerContext *controllercmd.ControllerContext,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	conditions []string,
	existingJobCompletionTimeout time.Duration) error {

	// Acquire a lock for this specific jobType/nodeName combination to prevent parallel execution
	jobName := jobType.GetJobName(nodeName)

	restartJobLocksMutex.Lock()
	jobLock, exists := restartJobLocks[jobName]
	if !exists {
		jobLock = &sync.Mutex{}
		restartJobLocks[jobName] = jobLock
	}
	restartJobLocksMutex.Unlock()

	jobLock.Lock()
	defer jobLock.Unlock()

	// Check if job already exists
	jobExists := true
	_, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, jobName, v1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing job %s: %w", jobName, err)
		}
		jobExists = false
	}

	// always try to run the controller, CEO might have been restarted
	RunTNFJobController(ctx, jobType, nodeName, controllerContext, operatorClient, kubeClient, kubeInformersForNamespaces, conditions)

	if !jobExists {
		// we are done
		return nil
	}

	// Job exists, wait for completion
	klog.Infof("Job %s already exists, waiting for being stopped", jobName)
	if err := WaitForStopped(ctx, kubeClient, jobName, operatorclient.TargetNamespace, existingJobCompletionTimeout); err != nil {
		return fmt.Errorf("failed to wait for update-setup job %s to complete: %w", jobName, err)
	}

	// Delete the job so the controller can recreate it
	klog.Infof("Deleting existing job %s", jobName)
	if err := DeleteAndWait(ctx, kubeClient, jobName, operatorclient.TargetNamespace); err != nil {
		return fmt.Errorf("failed to delete existing update-setup job %s: %w", jobName, err)
	}

	return nil
}

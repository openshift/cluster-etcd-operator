package operator

import (
	"context"
	"fmt"
	"sync"
	"time"

	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

// Local constants for lifecycle controller
const (
	// Controller name
	controllerNamePacemakerLifecycle = "PacemakerLifecycleManager"
)

// PacemakerLifecycleManager manages job controller startup for TNF clusters.
// Starts job controllers when conditions are met (bootstrap or runtime mode).
type pacemakerLifecycleManager struct {
	operatorClient    v1helpers.StaticPodOperatorClient
	kubeClient        kubernetes.Interface
	eventRecorder     events.Recorder
	pacemakerInformer cache.SharedIndexInformer

	// For node lifecycle management
	controlPlaneNodeInformer   cache.SharedIndexInformer
	controllerContext          *controllercmd.ControllerContext
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces
	etcdInformer               operatorv1informers.EtcdInformer

	// Job controller startup protection: prevents concurrent startJobControllers calls
	startJobControllersMu sync.Mutex
	// Track if job controllers have been started (set once, never reset)
	jobControllersStarted bool

	// Status collector startup protection: prevents duplicate status collector starts
	statusCollectorMu      sync.Mutex
	statusCollectorStarted bool

	// Health check controller startup protection: prevents duplicate health check starts
	healthCheckMu      sync.Mutex
	healthCheckStarted bool
}

// newPacemakerLifecycleManager creates a new PacemakerLifecycleManager for monitoring pacemaker status
// and managing node membership reconciliation in clusters that use ExternalEtcd.
// Returns the controller, the PacemakerLifecycleManager instance, and the PacemakerCluster informer
// (which must be started separately - see runPacemakerControllers in pkg/tnf/operator/starter.go).
func newPacemakerLifecycleManager(
	ctx context.Context,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	restConfig *rest.Config,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	controllerContext *controllercmd.ControllerContext,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) (factory.Controller, *pacemakerLifecycleManager, cache.SharedIndexInformer, error) {
	// Create PacemakerCluster informer
	informer, err := pacemaker.NewPacemakerClusterInformer(restConfig)
	if err != nil {
		return nil, nil, nil, err
	}

	c := &pacemakerLifecycleManager{
		operatorClient:             operatorClient,
		kubeClient:                 kubeClient,
		eventRecorder:              eventRecorder,
		pacemakerInformer:          informer,
		controlPlaneNodeInformer:   controlPlaneNodeInformer,
		controllerContext:          controllerContext,
		kubeInformersForNamespaces: kubeInformersForNamespaces,
		etcdInformer:               etcdInformer,
	}

	syncCtx := factory.NewSyncContext(controllerNamePacemakerLifecycle, eventRecorder.WithComponentSuffix("pacemaker-lifecycle-manager"))

	klog.Infof("%s controller created, waiting for informers to sync before starting", controllerNamePacemakerLifecycle)
	klog.Infof("PacemakerLifecycleManager will watch: operatorClient and %s/%s resource", pacmkrv1.SchemeGroupVersion.String(), pacemaker.PacemakerResourceName)

	// All informer events enqueue reconciliation; side effects run only in sync.
	controller := factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(time.Minute).
		WithSync(c.sync).
		WithSyncDegradedOnError(operatorClient).
		WithInformers(
			operatorClient.Informer(),
			informer,
			controlPlaneNodeInformer,
			etcdInformer.Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Pods().Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs().Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().CronJobs().Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Apps().V1().DaemonSets().Informer(),
		).ToController(controllerNamePacemakerLifecycle, syncCtx.Recorder())

	// PacemakerCluster informer is started in runPacemakerControllers (pkg/tnf/operator/starter.go)
	// Node informer is started in RunOperator (pkg/operator/starter.go)
	klog.Infof("PacemakerLifecycleManager controller created, will wait up to 10 minutes for informers to sync")

	return controller, c, informer, nil
}

// sync is the main sync function that gets called periodically to check pacemaker status
func (c *pacemakerLifecycleManager) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("PacemakerLifecycleManager sync started")
	defer klog.V(4).Infof("PacemakerLifecycleManager sync completed")

	// Start job controllers (runs in both bootstrap and runtime modes)
	if err := c.startJobControllers(ctx); err != nil {
		klog.Errorf("Failed to start job controllers: %v", err)
		return fmt.Errorf("failed to start job controllers: %w", err)
	}

	return c.reconcileGeneration(ctx)
}

// runPacemakerHealthCheckController starts the health check controller.
// Monitors Pacemaker cluster health via PacemakerCluster CR and sets operator Degraded conditions.
// Only starts if not already started (idempotent).
func (c *pacemakerLifecycleManager) runPacemakerHealthCheckController(ctx context.Context) {
	// Prevent duplicate starts
	c.healthCheckMu.Lock()
	if c.healthCheckStarted {
		c.healthCheckMu.Unlock()
		klog.V(4).Infof("Health check controller already started, skipping duplicate start")
		return
	}
	c.healthCheckMu.Unlock()

	healthCheckController, _, err := pacemaker.NewHealthCheckWithInformer(
		c.operatorClient,
		c.kubeClient,
		c.eventRecorder,
		c.pacemakerInformer,
	)
	if err != nil {
		klog.Errorf("Failed to create health check controller: %v", err)
		return
	}

	// Only set flag after successful creation
	c.healthCheckMu.Lock()
	c.healthCheckStarted = true
	c.healthCheckMu.Unlock()

	go healthCheckController.Run(ctx, 1)
	klog.Infof("Health check controller started")
}

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
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

	// Controller context for goroutines (set on first sync, cancelled on shutdown)
	controllerCtx   context.Context
	controllerCtxMu sync.Mutex
}

// newPacemakerLifecycleManager creates a new PacemakerLifecycleManager for monitoring pacemaker status
// and managing node membership reconciliation in clusters that use ExternalEtcd.
// Returns the controller, the PacemakerLifecycleManager instance, and the PacemakerCluster informer
// (which must be started separately - see runPacemakerControllers in pkg/tnf/operator/starter.go).
func newPacemakerLifecycleManager(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	restConfig *rest.Config,
	controlPlaneNodeInformer cache.SharedIndexInformer,
	controllerContext *controllercmd.ControllerContext,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) (factory.Controller, *pacemakerLifecycleManager, cache.SharedIndexInformer, error) {
	// Create REST client for PacemakerStatus CRs
	restClient, err := pacemaker.CreatePacemakerRESTClient(restConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create REST client: %w", err)
	}

	// Create scheme for the parameter codec
	scheme := runtime.NewScheme()
	if err := pacmkrv1.AddToScheme(scheme); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to add scheme for informer: %w", err)
	}

	// Create informer for PacemakerCluster
	klog.Infof("Creating PacemakerCluster informer for group %s, resource %s", pacmkrv1.SchemeGroupVersion.String(), pacemaker.PacemakerResourceName)
	informer := cache.NewSharedIndexInformer(
		&pacemaker.PacemakerListWatch{ListWatch: cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				klog.V(4).Infof("PacemakerCluster informer ListFunc called for resource %s", pacemaker.PacemakerResourceName)
				sanitizedOptions := pacemaker.SanitizeListOptions(options)
				result := &pacmkrv1.PacemakerClusterList{}
				err := restClient.Get().
					Resource(pacemaker.PacemakerResourceName).
					VersionedParams(&sanitizedOptions, runtime.NewParameterCodec(scheme)).
					Do(context.Background()).
					Into(result)
				if err != nil {
					klog.Errorf("Failed to list PacemakerCluster resources (%s): %v", pacemaker.PacemakerResourceName, err)
				} else {
					klog.V(4).Infof("Successfully listed PacemakerCluster resources, found %d items", len(result.Items))
				}
				return result, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				klog.V(4).Infof("PacemakerCluster informer WatchFunc called for resource %s", pacemaker.PacemakerResourceName)
				sanitizedOptions := pacemaker.SanitizeListOptions(options)
				watcher, err := restClient.Get().
					Resource(pacemaker.PacemakerResourceName).
					VersionedParams(&sanitizedOptions, runtime.NewParameterCodec(scheme)).
					Watch(context.Background())
				if err != nil {
					klog.Errorf("Failed to watch PacemakerCluster resources (%s): %v", pacemaker.PacemakerResourceName, err)
				}
				return watcher, err
			},
		}},
		&pacmkrv1.PacemakerCluster{},
		pacemaker.HealthCheckResyncInterval,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

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

	// ResyncEvery ensures the sync function is called at regular intervals (1 minute)
	// even if no informer events are detected.
	controller := factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(time.Minute).
		WithSync(c.sync).
		WithInformers(
			operatorClient.Informer(),
			informer,
			controlPlaneNodeInformer,
		).ToController(controllerNamePacemakerLifecycle, syncCtx.Recorder())

	// PacemakerCluster informer is started in runPacemakerControllers (pkg/tnf/operator/starter.go)
	// Node informer is started in RunOperator (pkg/operator/starter.go)
	klog.Infof("PacemakerLifecycleManager controller created, will wait up to 10 minutes for informers to sync")

	// Register node event handlers for drift-triggered reconciliation
	if err := c.registerNodeEventHandlers(); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to register node event handlers: %w", err)
	}

	return controller, c, informer, nil
}

// registerNodeEventHandlers registers Update event handler on the node informer.
// Returns an error if registration fails, as the lifecycle manager cannot function without
// receiving node events for update-setup job restarts.
// UpdateFunc handles Ready transitions to trigger update-setup job restart (post-transition only).
func (c *pacemakerLifecycleManager) registerNodeEventHandlers() error {
	if c.controlPlaneNodeInformer == nil {
		return fmt.Errorf("controlPlaneNodeInformer is nil")
	}

	_, err := c.controlPlaneNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			oldNode, oldOk := oldObj.(*corev1.Node)
			newNode, newOk := newObj.(*corev1.Node)
			if !oldOk || !newOk {
				klog.Warningf("failed to convert updated object to Node, old=%+v, new=%+v", oldObj, newObj)
				return
			}

			// Check for Ready transition - triggers update-setup restart post-transition
			oldReady := tools.IsNodeReady(oldNode)
			newReady := tools.IsNodeReady(newNode)
			if !oldReady && newReady {
				klog.Infof("node %s transitioned to ready state - restarting update-setup job", newNode.GetName())
				go func() {
					// Use controller context (cancelled on shutdown) instead of background context
					c.controllerCtxMu.Lock()
					ctx := c.controllerCtx
					c.controllerCtxMu.Unlock()

					if ctx == nil {
						// Controller hasn't started yet, event fired before first sync
						// This shouldn't happen (informers sync before events fire), but be defensive
						klog.V(4).Infof("Skipping node ready event handler - controller context not yet available")
						return
					}

					// Restart update-setup job when nodes become ready (e.g., after replacement)
					// This ensures update-setup reruns if it completed before auth ran on new node
					if err := c.restartUpdateSetupJob(ctx); err != nil {
						klog.Errorf("Failed to restart update-setup job on node ready: %v", err)
					}
				}()
			}
		},
	})

	if err != nil {
		return fmt.Errorf("failed to add event handler to node informer: %w", err)
	}

	klog.Infof("Registered Update event handler for node lifecycle management")
	return nil
}

// sync is the main sync function that gets called periodically to check pacemaker status
func (c *pacemakerLifecycleManager) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("PacemakerLifecycleManager sync started")
	defer klog.V(4).Infof("PacemakerLifecycleManager sync completed")

	// Store controller context on first sync (for event handler goroutines)
	c.controllerCtxMu.Lock()
	if c.controllerCtx == nil {
		c.controllerCtx = ctx
	}
	c.controllerCtxMu.Unlock()

	// Start job controllers (runs in both bootstrap and runtime modes)
	if err := c.startJobControllers(ctx); err != nil {
		klog.Errorf("Failed to start job controllers: %v", err)
		return fmt.Errorf("failed to start job controllers: %w", err)
	}

	return nil
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
	c.healthCheckStarted = true
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

	go healthCheckController.Run(ctx, 1)
	klog.Infof("Health check controller started")
}

// restartUpdateSetupJob restarts the update-setup job controller when nodes become ready.
// Only runs post-transition when exactly 2 control plane nodes exist.
func (c *pacemakerLifecycleManager) restartUpdateSetupJob(ctx context.Context) error {
	// Check if transition is complete - update-setup only runs post-transition
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, c.operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check external etcd transition status: %w", err)
	}
	if !transitionComplete {
		klog.V(4).Infof("Skipping update-setup restart - transition not yet complete")
		return nil
	}

	// Get control plane nodes
	if c.controlPlaneNodeInformer == nil || !c.controlPlaneNodeInformer.HasSynced() {
		klog.V(4).Infof("Skipping update-setup restart - node informer not synced yet")
		return nil
	}
	controlPlaneNodes, err := tools.ListNodesFromInformer(c.controlPlaneNodeInformer)
	if err != nil {
		return fmt.Errorf("failed to list control plane nodes: %w", err)
	}

	// Update-setup requires exactly 2 nodes (pacemaker limitation)
	if len(controlPlaneNodes) != 2 {
		klog.V(4).Infof("Skipping update-setup restart: requires exactly 2 control plane nodes, have %d", len(controlPlaneNodes))
		return nil
	}

	// schedulableNodesFunc: returns ready nodes where job can run (K8s ∩ Pacemaker intersection)
	schedulableNodesFunc := func() ([]*corev1.Node, error) {
		return c.getActivePacemakerNodes()
	}

	// affectedNodesFunc: all control plane nodes (ready or not)
	// Job waits for these nodes to become ready before proceeding
	updateSetupAffectedNodesFunc := func() ([]*corev1.Node, error) {
		return tools.ListNodesFromInformer(c.controlPlaneNodeInformer)
	}

	klog.Infof("Restarting update-setup job controller after node ready event")
	if err := jobs.RestartClusterJobOrRunController(
		ctx,
		tools.JobTypeUpdateSetup,
		schedulableNodesFunc,
		updateSetupAffectedNodesFunc,
		nil, // no jobConfigFunc for update-setup
		3,   // retries
		c.controllerContext,
		c.operatorClient,
		c.kubeClient,
		c.kubeInformersForNamespaces,
		jobs.DefaultConditions,
		10*time.Second, // existingJobCompletionTimeout
	); err != nil {
		return fmt.Errorf("failed to restart update-setup job controller: %w", err)
	}

	return nil
}

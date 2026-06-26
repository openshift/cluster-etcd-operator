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
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// Local constants for lifecycle controller
const (
	// Controller name
	controllerNamePacemakerLifecycle = "PacemakerLifecycleManager"
)

// PacemakerLifecycleManager monitors pacemaker status and manages node membership reconciliation
// in ExternalEtcd topology clusters. It handles health monitoring, drift detection, and
// triggers update-setup jobs when pacemaker membership diverges from Kubernetes.
type PacemakerLifecycleManager struct {
	operatorClient    v1helpers.StaticPodOperatorClient
	kubeClient        kubernetes.Interface
	eventRecorder     events.Recorder
	pacemakerInformer cache.SharedIndexInformer

	// For node lifecycle management
	nodeInformer               cache.SharedIndexInformer
	controllerContext          *controllercmd.ControllerContext
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces
	etcdInformer               operatorv1informers.EtcdInformer

	// Event deduplication: tracks recently recorded events to avoid duplicates
	recordedEventsMu sync.Mutex
	recordedEvents   map[string]time.Time

	// Previous health status for transition detection and grace period calculation.
	// Only updated when status is non-Unknown, so CRLastUpdated reflects the last valid CR timestamp.
	previousMu sync.Mutex
	previous   *pacemaker.HealthStatus

	// Job controller startup protection: prevents concurrent StartJobControllers calls
	startJobControllersMu sync.Mutex

	// Lifecycle context for goroutines spawned by event handlers
	lifecycleCtx       context.Context
	lifecycleCtxCancel context.CancelFunc
}

// NewPacemakerLifecycleManager creates a new PacemakerLifecycleManager for monitoring pacemaker status
// and managing node membership reconciliation in clusters that use ExternalEtcd.
// Returns the controller, the PacemakerLifecycleManager instance, and the PacemakerCluster informer
// (which must be started separately - see runPacemakerControllers in pkg/tnf/operator/starter.go).
func NewPacemakerLifecycleManager(
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	restConfig *rest.Config,
	nodeInformer cache.SharedIndexInformer,
	controllerContext *controllercmd.ControllerContext,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	etcdInformer operatorv1informers.EtcdInformer,
) (factory.Controller, *PacemakerLifecycleManager, cache.SharedIndexInformer, error) {
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
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				klog.V(4).Infof("PacemakerCluster informer ListFunc called for resource %s", pacemaker.PacemakerResourceName)
				// Sanitize ListOptions - remove fields not supported by all Kubernetes versions
				sanitizedOptions := metav1.ListOptions{
					LabelSelector:       options.LabelSelector,
					FieldSelector:       options.FieldSelector,
					Watch:               options.Watch,
					AllowWatchBookmarks: options.AllowWatchBookmarks,
					ResourceVersion:     options.ResourceVersion,
					TimeoutSeconds:      options.TimeoutSeconds,
					Limit:               options.Limit,
					Continue:            options.Continue,
				}
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
				// Sanitize ListOptions - remove fields not supported by all Kubernetes versions
				// sendInitialEvents and resourceVersionMatch were added in 1.27+ and cause errors on older clusters
				sanitizedOptions := metav1.ListOptions{
					LabelSelector:       options.LabelSelector,
					FieldSelector:       options.FieldSelector,
					Watch:               options.Watch,
					AllowWatchBookmarks: options.AllowWatchBookmarks,
					ResourceVersion:     options.ResourceVersion,
					TimeoutSeconds:      options.TimeoutSeconds,
					Limit:               options.Limit,
					Continue:            options.Continue,
				}
				watcher, err := restClient.Get().
					Resource(pacemaker.PacemakerResourceName).
					VersionedParams(&sanitizedOptions, runtime.NewParameterCodec(scheme)).
					Watch(context.Background())
				if err != nil {
					klog.Errorf("Failed to watch PacemakerCluster resources (%s): %v", pacemaker.PacemakerResourceName, err)
				}
				return watcher, err
			},
		},
		&pacmkrv1.PacemakerCluster{},
		pacemaker.HealthCheckResyncInterval,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	// Create lifecycle context for goroutines spawned by event handlers
	lifecycleCtx, lifecycleCtxCancel := context.WithCancel(context.Background())

	c := &PacemakerLifecycleManager{
		operatorClient:             operatorClient,
		kubeClient:                 kubeClient,
		eventRecorder:              eventRecorder,
		pacemakerInformer:          informer,
		nodeInformer:               nodeInformer,
		controllerContext:          controllerContext,
		kubeInformersForNamespaces: kubeInformersForNamespaces,
		etcdInformer:               etcdInformer,
		recordedEvents:             make(map[string]time.Time),
		lifecycleCtx:               lifecycleCtx,
		lifecycleCtxCancel:         lifecycleCtxCancel,
	}

	// Initialize update-setup generation counter from existing ConfigMaps
	// This prevents reusing stale ConfigMaps after operator restart
	if err := c.initUpdateSetupGeneration(context.Background()); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to initialize update-setup generation counter: %w", err)
	}

	syncCtx := factory.NewSyncContext(controllerNamePacemakerLifecycle, eventRecorder.WithComponentSuffix("pacemaker-lifecycle-manager"))

	klog.Infof("%s controller created, waiting for informers to sync before starting", controllerNamePacemakerLifecycle)
	klog.Infof("PacemakerLifecycleManager will watch: operatorClient and %s/%s resource", pacmkrv1.SchemeGroupVersion.String(), pacemaker.PacemakerResourceName)

	// ResyncEvery ensures the sync function is called at regular intervals (30s)
	// even if no informer events are detected.
	controller := factory.New().
		WithSyncContext(syncCtx).
		ResyncEvery(pacemaker.HealthCheckResyncInterval).
		WithSync(c.sync).
		WithInformers(
			operatorClient.Informer(),
			informer,
			nodeInformer,
		).ToController(controllerNamePacemakerLifecycle, syncCtx.Recorder())

	// PacemakerCluster informer is started in runPacemakerControllers (pkg/tnf/operator/starter.go)
	// Node informer is started in RunOperator (pkg/operator/starter.go)
	klog.Infof("PacemakerLifecycleManager controller created, will wait up to 10 minutes for informers to sync")

	// Register node event handlers for drift-triggered reconciliation
	if err := c.RegisterNodeEventHandlers(); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to register node event handlers: %w", err)
	}

	return controller, c, informer, nil
}

// RegisterNodeEventHandlers registers Add/Update/Delete event handlers on the node informer.
// Returns an error if registration fails, as the lifecycle manager cannot function without
// receiving node events for drift detection and reconciliation.
// - UpdateFunc: handles Ready transitions for initial bootstrap and IP changes while Ready
// - AddFunc/DeleteFunc: trigger drift detection and reconciliation
func (c *PacemakerLifecycleManager) RegisterNodeEventHandlers() error {
	if c.nodeInformer == nil {
		return fmt.Errorf("nodeInformer is nil")
	}

	_, err := c.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			oldNode, oldOk := oldObj.(*corev1.Node)
			newNode, newOk := newObj.(*corev1.Node)
			if !oldOk || !newOk {
				klog.Warningf("failed to convert updated object to Node, old=%+v, new=%+v", oldObj, newObj)
				return
			}

			// Check for Ready transition (bootstrap path)
			oldReady := tools.IsNodeReady(oldNode)
			newReady := tools.IsNodeReady(newNode)
			if !oldReady && newReady {
				klog.Infof("node %s transitioned to ready state - triggering job controller startup", newNode.GetName())
				go func() {
					if err := c.StartJobControllers(c.lifecycleCtx); err != nil {
						klog.Errorf("Failed to start job controllers on node ready: %v", err)
					}
				}()
			}

			// Check if node IP changed while Ready - trigger reconciliation
			if newReady {
				oldIP, oldErr := tools.GetNodeIPForPacemaker(*oldNode)
				newIP, newErr := tools.GetNodeIPForPacemaker(*newNode)
				if oldErr == nil && newErr == nil && oldIP != newIP {
					klog.Infof("node %s IP changed from %s to %s - triggering reconciliation check", newNode.GetName(), oldIP, newIP)
					go func() {
						if err := c.ReconcilePacemakerConfig(c.lifecycleCtx); err != nil {
							klog.Errorf("Failed to ensure reconciliation on IP change: %v", err)
						}
					}()
				}
			}
		},
		AddFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				klog.Warningf("failed to convert added object to Node %+v", obj)
				return
			}
			klog.Infof("node added: %s - triggering reconciliation check", node.GetName())
			// Trigger reconciliation check (will no-op if transition not complete, no drift, or already running)
			go func() {
				if err := c.ReconcilePacemakerConfig(c.lifecycleCtx); err != nil {
					klog.Errorf("Failed to ensure reconciliation on node add: %v", err)
				}
			}()
		},
		DeleteFunc: func(obj any) {
			node, ok := obj.(*corev1.Node)
			if !ok {
				// Handle tombstone case
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.Warningf("failed to convert deleted object to Node or tombstone %+v", obj)
					return
				}
				node, ok = tombstone.Obj.(*corev1.Node)
				if !ok {
					klog.Warningf("tombstone contained object that is not a Node %+v", obj)
					return
				}
			}
			klog.Infof("node deleted: %s - triggering reconciliation check", node.GetName())
			// Trigger reconciliation check (will no-op if transition not complete, no drift, or already running)
			go func() {
				if err := c.ReconcilePacemakerConfig(c.lifecycleCtx); err != nil {
					klog.Errorf("Failed to ensure reconciliation on node delete: %v", err)
				}
			}()
		},
	})

	if err != nil {
		return fmt.Errorf("failed to add event handler to node informer: %w", err)
	}

	klog.Infof("Registered Add/Update/Delete event handlers for node lifecycle management")
	return nil
}

// sync is the main sync function that gets called periodically to check pacemaker status
func (c *PacemakerLifecycleManager) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("PacemakerLifecycleManager sync started")
	defer klog.V(4).Infof("PacemakerLifecycleManager sync completed")

	// Check if external etcd transition is complete
	transitionComplete, err := ceohelpers.HasExternalEtcdCompletedTransition(ctx, c.operatorClient)
	if err != nil {
		return fmt.Errorf("failed to check external etcd transition status: %w", err)
	}

	// Aggregate errors across independent operations
	// Each operation continues even if others fail, ensuring partial progress
	var errs []error

	// 1. Start job controllers (runs in both modes with different behavior)
	if err := c.StartJobControllers(ctx); err != nil {
		klog.Errorf("Failed to start job controllers: %v", err)
		errs = append(errs, fmt.Errorf("failed to start job controllers: %w", err))
	}

	// 2. Cleanup orphaned and old jobs (always runs, even before transition)
	// This prevents job/pod accumulation during bootstrap or unhealthy states
	if err := c.CleanupOrphanedJobs(ctx); err != nil {
		klog.Errorf("Failed to cleanup orphaned jobs: %v", err)
		errs = append(errs, fmt.Errorf("failed to cleanup orphaned jobs: %w", err))
	}

	// Operations 3-4 only run after external etcd transition completes
	if transitionComplete {
		// 3. Monitor pacemaker health
		if err := c.MonitorHealth(ctx); err != nil {
			klog.Errorf("Failed to monitor health: %v", err)
			errs = append(errs, fmt.Errorf("failed to monitor health: %w", err))
		}

		// 4. Reconcile pacemaker configuration (drift detection)
		if err := c.ReconcilePacemakerConfig(ctx); err != nil {
			klog.Errorf("Failed to reconcile pacemaker config: %v", err)
			errs = append(errs, fmt.Errorf("failed to reconcile pacemaker config: %w", err))
		}
	}

	// Return aggregated errors - controller framework will mark degraded and retry
	return errors.NewAggregate(errs)
}

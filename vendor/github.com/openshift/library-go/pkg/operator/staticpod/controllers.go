package staticpod

import (
	"fmt"
	"time"

	"k8s.io/utils/clock"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/manager"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/revisioncontroller"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/backingresource"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/guard"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installerstate"
	missingstaticpodcontroller "github.com/openshift/library-go/pkg/operator/staticpod/controller/missingstaticpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/node"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/prune"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/startupmonitorcondition"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/staticpodfallback"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/staticpodstate"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/unsupportedconfigoverridescontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

type staticPodOperatorControllerBuilder struct {
	// clients and related
	staticPodOperatorClient v1helpers.StaticPodOperatorClient
	kubeClient              kubernetes.Interface
	kubeNamespaceInformers  v1helpers.KubeInformersForNamespaces
	kubeClusterInformers    informers.SharedInformerFactory
	configInformers         externalversions.SharedInformerFactory
	clock                   clock.Clock
	eventRecorder           events.Recorder

	// resource information
	operandNamespace        string
	staticPodName           string
	operandPodLabelSelector labels.Selector
	revisionConfigMaps      []revisioncontroller.RevisionResource
	revisionSecrets         []revisioncontroller.RevisionResource

	// cert information
	certDir        string
	certConfigMaps []installer.UnrevisionedResource
	certSecrets    []installer.UnrevisionedResource

	// versioner information
	versionRecorder status.VersionGetter
	operandName     string

	// installer information
	installCommand           []string
	installerPodMutationFunc installer.InstallerPodMutationFunc
	minReadyDuration         time.Duration
	enableStartMonitor       func() (bool, error)

	// pruning information
	pruneCommand []string
	// TODO de-dupe this.  I think it's actually a directory name
	staticPodPrefix string

	// guard infomation
	operatorName                  string
	operatorNamespace             string
	readyzPort                    string
	readyzEndpoint                string
	pdbUnhealthyPodEvictionPolicy *v1.UnhealthyPodEvictionPolicyType
	guardCreateConditionalFunc    func() (bool, bool, error)

	revisionControllerPrecondition revisioncontroller.PreconditionFunc

	extraNodeSelector labels.Selector
}

func NewBuilder(
	staticPodOperatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeNamespaceInformers v1helpers.KubeInformersForNamespaces,
	clusterInformers informers.SharedInformerFactory,
	configInformers externalversions.SharedInformerFactory,
	clock clock.Clock,
) Builder {
	return &staticPodOperatorControllerBuilder{
		staticPodOperatorClient: staticPodOperatorClient,
		kubeClient:              kubeClient,
		kubeNamespaceInformers:  kubeNamespaceInformers,
		kubeClusterInformers:    clusterInformers,
		configInformers:         configInformers,
		clock:                   clock,
	}
}

// Builder allows the caller to construct a set of static pod controllers in pieces
type Builder interface {
	WithEvents(eventRecorder events.Recorder) Builder
	WithVersioning(operandName string, versionRecorder status.VersionGetter) Builder
	WithOperandPodLabelSelector(labelSelector labels.Selector) Builder
	WithRevisionedResources(operandNamespace, staticPodName string, revisionConfigMaps, revisionSecrets []revisioncontroller.RevisionResource) Builder
	WithUnrevisionedCerts(certDir string, certConfigMaps, certSecrets []installer.UnrevisionedResource) Builder
	WithInstaller(command []string) Builder
	WithMinReadyDuration(minReadyDuration time.Duration) Builder
	WithStartupMonitor(enabledStartupMonitor func() (bool, error)) Builder

	// WithExtraNodeSelector Informs controllers to handle extra nodes as well as master nodes.
	WithExtraNodeSelector(extraNodeSelector labels.Selector) Builder

	// WithCustomInstaller allows mutating the installer pod definition just before
	// the installer pod is created for a revision.
	WithCustomInstaller(command []string, installerPodMutationFunc installer.InstallerPodMutationFunc) Builder
	WithPruning(command []string, staticPodPrefix string) Builder

	// WithPodDisruptionBudgetGuard manages guard pods and high available pod disruption budget
	//
	// optionally pdbUnhealthyPodEvictionPolicy can be set to AlwaysAllow to allows eviction of unhealthy (not ready) pods
	// even if there are no disruptions allowed on a PodDisruptionBudget.
	// This can help to drain/maintain a node and recover without a manual intervention when multiple instances of nodes or pods are misbehaving.
	// Use this with caution, as this option can disrupt perspective pods that have not yet had a chance to become healthy.
	WithPodDisruptionBudgetGuard(operatorNamespace, operatorName, readyzPort, readyzEndpoint string, pdbUnhealthyPodEvictionPolicy *v1.UnhealthyPodEvictionPolicyType, createConditionalFunc func() (bool, bool, error)) Builder
	WithRevisionControllerPrecondition(revisionControllerPrecondition revisioncontroller.PreconditionFunc) Builder
	ToControllers() (manager.ControllerManager, error)
}

func (b *staticPodOperatorControllerBuilder) WithExtraNodeSelector(extraNodeSelector labels.Selector) Builder {
	b.extraNodeSelector = extraNodeSelector
	return b
}

func (b *staticPodOperatorControllerBuilder) WithEvents(eventRecorder events.Recorder) Builder {
	b.eventRecorder = eventRecorder
	return b
}

func (b *staticPodOperatorControllerBuilder) WithVersioning(operandName string, versionRecorder status.VersionGetter) Builder {
	b.operandName = operandName
	b.versionRecorder = versionRecorder
	return b
}

func (b *staticPodOperatorControllerBuilder) WithOperandPodLabelSelector(labelSelector labels.Selector) Builder {
	b.operandPodLabelSelector = labelSelector
	return b
}

func (b *staticPodOperatorControllerBuilder) WithRevisionedResources(operandNamespace, staticPodName string, revisionConfigMaps, revisionSecrets []revisioncontroller.RevisionResource) Builder {
	b.operandNamespace = operandNamespace
	b.staticPodName = staticPodName
	b.revisionConfigMaps = revisionConfigMaps
	b.revisionSecrets = revisionSecrets
	return b
}

func (b *staticPodOperatorControllerBuilder) WithUnrevisionedCerts(certDir string, certConfigMaps, certSecrets []installer.UnrevisionedResource) Builder {
	b.certDir = certDir
	b.certConfigMaps = certConfigMaps
	b.certSecrets = certSecrets
	return b
}

func (b *staticPodOperatorControllerBuilder) WithInstaller(command []string) Builder {
	b.installCommand = command
	b.installerPodMutationFunc = func(pod *corev1.Pod, nodeName string, operatorSpec *operatorv1.StaticPodOperatorSpec, revision int32) error {
		return nil
	}
	return b
}

func (b *staticPodOperatorControllerBuilder) WithMinReadyDuration(minReadyDuration time.Duration) Builder {
	b.minReadyDuration = minReadyDuration
	return b
}

func (b *staticPodOperatorControllerBuilder) WithStartupMonitor(enabledStartupMonitor func() (bool, error)) Builder {
	b.enableStartMonitor = enabledStartupMonitor
	return b
}

// WithCustomInstaller allows mutating the installer pod definition just before
// the installer pod is created for a revision.
func (b *staticPodOperatorControllerBuilder) WithCustomInstaller(command []string, installerPodMutationFunc installer.InstallerPodMutationFunc) Builder {
	b.installCommand = command
	b.installerPodMutationFunc = installerPodMutationFunc
	return b
}

func (b *staticPodOperatorControllerBuilder) WithPruning(command []string, staticPodPrefix string) Builder {
	b.pruneCommand = command
	b.staticPodPrefix = staticPodPrefix
	return b
}

// WithPodDisruptionBudgetGuard manages guard pods and high available pod disruption budget
//
// optionally pdbUnhealthyPodEvictionPolicy can be set to AlwaysAllow to allows eviction of unhealthy (not ready) pods
// even if there are no disruptions allowed on a PodDisruptionBudget.
// This can help to drain/maintain a node and recover without a manual intervention when multiple instances of nodes or pods are misbehaving.
// Use this with caution, as this option can disrupt perspective pods that have not yet had a chance to become healthy.
func (b *staticPodOperatorControllerBuilder) WithPodDisruptionBudgetGuard(operatorNamespace, operatorName, readyzPort, readyzEndpoint string, pdbUnhealthyPodEvictionPolicy *v1.UnhealthyPodEvictionPolicyType, createConditionalFunc func() (bool, bool, error)) Builder {
	b.operatorNamespace = operatorNamespace
	b.operatorName = operatorName
	b.readyzPort = readyzPort
	b.readyzEndpoint = readyzEndpoint
	b.pdbUnhealthyPodEvictionPolicy = pdbUnhealthyPodEvictionPolicy
	b.guardCreateConditionalFunc = createConditionalFunc
	return b
}

func (b *staticPodOperatorControllerBuilder) WithRevisionControllerPrecondition(revisionControllerPrecondition revisioncontroller.PreconditionFunc) Builder {
	b.revisionControllerPrecondition = revisionControllerPrecondition
	return b
}

func (b *staticPodOperatorControllerBuilder) ToControllers() (manager.ControllerManager, error) {
	manager := manager.NewControllerManager()

	eventRecorder := b.eventRecorder
	if eventRecorder == nil {
		eventRecorder = events.NewLoggingEventRecorder("static-pod-operator-controller", b.clock)
	}
	versionRecorder := b.versionRecorder
	if versionRecorder == nil {
		versionRecorder = status.NewVersionGetter()
	}

	// ensure that all controllers that need the secret/configmap informer-based clients
	// need to wait for their synchronization before starting using WithInformer
	configMapClient := v1helpers.CachedConfigMapGetter(b.kubeClient.CoreV1(), b.kubeNamespaceInformers)
	secretClient := v1helpers.CachedSecretGetter(b.kubeClient.CoreV1(), b.kubeNamespaceInformers)
	podClient := b.kubeClient.CoreV1()
	eventsClient := b.kubeClient.CoreV1()
	pdbClient := b.kubeClient.PolicyV1()
	operandInformers := b.kubeNamespaceInformers.InformersFor(b.operandNamespace)
	clusterInformers := b.kubeClusterInformers
	infraInformers := b.configInformers.Config().V1().Infrastructures()

	var errs []error

	if len(b.operandNamespace) > 0 {
		manager.WithController(revisioncontroller.NewRevisionController(
			b.operandName,
			b.operandNamespace,
			b.revisionConfigMaps,
			b.revisionSecrets,
			operandInformers,
			b.staticPodOperatorClient,
			configMapClient,
			secretClient,
			eventRecorder,
			b.revisionControllerPrecondition,
		), 1)
	} else {
		errs = append(errs, fmt.Errorf("missing revisionController; cannot proceed"))
	}

	if len(b.installCommand) > 0 {
		manager.WithController(installer.NewInstallerController(
			b.operandName,
			b.operandNamespace,
			b.staticPodName,
			b.revisionConfigMaps,
			b.revisionSecrets,
			b.installCommand,
			operandInformers,
			b.staticPodOperatorClient,
			configMapClient,
			secretClient,
			podClient,
			eventRecorder,
		).WithCerts(
			b.certDir,
			b.certConfigMaps,
			b.certSecrets,
		).WithInstallerPodMutationFn(
			b.installerPodMutationFunc,
		).WithMinReadyDuration(
			b.minReadyDuration,
		), 1)

		manager.WithController(installerstate.NewInstallerStateController(
			b.operandName,
			operandInformers,
			podClient,
			eventsClient,
			b.staticPodOperatorClient,
			b.operandNamespace,
			eventRecorder,
		), 1)
	} else {
		errs = append(errs, fmt.Errorf("missing installerController; cannot proceed"))
	}

	if len(b.operandName) > 0 {
		// TODO add handling for operator configmap changes to get version-mapping changes
		manager.WithController(staticpodstate.NewStaticPodStateController(
			b.operandName,
			b.operandNamespace,
			b.staticPodName,
			b.operandName,
			operandInformers,
			b.staticPodOperatorClient,
			podClient,
			versionRecorder,
			eventRecorder,
		), 1)
	} else {
		eventRecorder.Warning("StaticPodStateControllerMissing", "not enough information provided, not all functionality is present")
	}

	if len(b.pruneCommand) > 0 {
		manager.WithController(prune.NewPruneController(
			b.operandNamespace,
			b.staticPodPrefix,
			b.pruneCommand,
			configMapClient,
			podClient,
			b.staticPodOperatorClient,
			operandInformers,
			eventRecorder,
		), 1)
	} else {
		eventRecorder.Warning("PruningControllerMissing", "not enough information provided, not all functionality is present")
	}

	if b.enableStartMonitor != nil {
		manager.WithController(startupmonitorcondition.New(
			b.operandName,
			b.operandNamespace,
			b.staticPodName,
			b.staticPodOperatorClient,
			b.kubeNamespaceInformers,
			b.enableStartMonitor,
			eventRecorder,
		), 1)

		if staticPodFallbackController, err := staticpodfallback.New(
			b.operandName,
			b.operandNamespace,
			b.operandPodLabelSelector,
			b.staticPodOperatorClient,
			b.kubeNamespaceInformers,
			b.enableStartMonitor,
			b.eventRecorder,
		); err == nil {
			manager.WithController(staticPodFallbackController, 1)
		} else {
			errs = append(errs, err)
		}
	}

	manager.WithController(node.NewNodeController(
		b.operandName,
		b.staticPodOperatorClient,
		clusterInformers,
		eventRecorder,
		b.extraNodeSelector,
	), 1)

	// this cleverly sets the same condition that used to be set because of the way that the names are constructed
	manager.WithController(staticresourcecontroller.NewStaticResourceController(
		"BackingResourceController",
		backingresource.StaticPodManifests(b.operandNamespace),
		[]string{
			"manifests/installer-sa.yaml",
			"manifests/installer-cluster-rolebinding.yaml",
		},
		resourceapply.NewKubeClientHolder(b.kubeClient),
		b.staticPodOperatorClient,
		eventRecorder,
	).AddKubeInformers(b.kubeNamespaceInformers), 1)

	manager.WithController(unsupportedconfigoverridescontroller.NewUnsupportedConfigOverridesController(b.operatorName, b.staticPodOperatorClient, eventRecorder), 1)
	manager.WithController(loglevel.NewClusterOperatorLoggingController(b.staticPodOperatorClient, eventRecorder), 1)

	if len(b.operatorNamespace) > 0 && len(b.operatorName) > 0 && len(b.readyzPort) > 0 && len(b.readyzEndpoint) > 0 {
		if guardController, err := guard.NewGuardController(
			b.operandNamespace,
			b.operandPodLabelSelector,
			b.staticPodName,
			b.operatorName,
			b.readyzPort,
			b.readyzEndpoint,
			b.pdbUnhealthyPodEvictionPolicy,
			operandInformers,
			clusterInformers,
			b.staticPodOperatorClient,
			podClient,
			pdbClient,
			eventRecorder,
			b.guardCreateConditionalFunc,
			b.extraNodeSelector,
		); err == nil {
			manager.WithController(guardController, 1)
		} else {
			errs = append(errs, err)
		}
	}

	manager.WithController(missingstaticpodcontroller.New(
		b.staticPodOperatorClient,
		b.kubeNamespaceInformers.InformersFor(b.operandNamespace),
		b.eventRecorder,
		b.operandNamespace,
		b.staticPodName,
		b.operandName,
		infraInformers,
	), 1)

	return manager, errors.NewAggregate(errs)
}

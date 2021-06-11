package staticpod

import (
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/manager"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/revisioncontroller"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/backingresource"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installerstate"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/node"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/prune"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/staticpodstate"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/unsupportedconfigoverridescontroller"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
)

type staticPodOperatorControllerBuilder struct {
	// clients and related
	staticPodOperatorClient v1helpers.StaticPodOperatorClient
	kubeClient              kubernetes.Interface
	kubeInformers           v1helpers.KubeInformersForNamespaces
	eventRecorder           events.Recorder

	// resource information
	operandNamespace   string
	staticPodName      string
	revisionConfigMaps []revisioncontroller.RevisionResource
	revisionSecrets    []revisioncontroller.RevisionResource

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

	// pruning information
	pruneCommand []string
	// TODO de-dupe this.  I think it's actually a directory name
	staticPodPrefix string
}

func NewBuilder(
	staticPodOperatorClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
) Builder {
	return &staticPodOperatorControllerBuilder{
		staticPodOperatorClient: staticPodOperatorClient,
		kubeClient:              kubeClient,
		kubeInformers:           kubeInformers,
	}
}

// Builder allows the caller to construct a set of static pod controllers in pieces
type Builder interface {
	WithEvents(eventRecorder events.Recorder) Builder
	WithVersioning(operandName string, versionRecorder status.VersionGetter) Builder
	WithRevisionedResources(operandNamespace, staticPodName string, revisionConfigMaps, revisionSecrets []revisioncontroller.RevisionResource) Builder
	WithUnrevisionedCerts(certDir string, certConfigMaps, certSecrets []installer.UnrevisionedResource) Builder
	WithInstaller(command []string) Builder
	WithMinReadyDuration(minReadyDuration time.Duration) Builder
	// WithCustomInstaller allows mutating the installer pod definition just before
	// the installer pod is created for a revision.
	WithCustomInstaller(command []string, installerPodMutationFunc installer.InstallerPodMutationFunc) Builder
	WithPruning(command []string, staticPodPrefix string) Builder
	ToControllers() (manager.ControllerManager, error)
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

func (b *staticPodOperatorControllerBuilder) ToControllers() (manager.ControllerManager, error) {
	manager := manager.NewControllerManager()

	eventRecorder := b.eventRecorder
	if eventRecorder == nil {
		eventRecorder = events.NewLoggingEventRecorder("static-pod-operator-controller")
	}
	versionRecorder := b.versionRecorder
	if versionRecorder == nil {
		versionRecorder = status.NewVersionGetter()
	}

	configMapClient := v1helpers.CachedConfigMapGetter(b.kubeClient.CoreV1(), b.kubeInformers)
	secretClient := v1helpers.CachedSecretGetter(b.kubeClient.CoreV1(), b.kubeInformers)
	podClient := b.kubeClient.CoreV1()
	eventsClient := b.kubeClient.CoreV1()
	operandInformers := b.kubeInformers.InformersFor(b.operandNamespace)
	clusterInformers := b.kubeInformers.InformersFor("")

	var errs []error

	if len(b.operandNamespace) > 0 {
		manager.WithController(revisioncontroller.NewRevisionController(
			b.operandNamespace,
			b.revisionConfigMaps,
			b.revisionSecrets,
			operandInformers,
			revisioncontroller.StaticPodLatestRevisionClient{StaticPodOperatorClient: b.staticPodOperatorClient},
			configMapClient,
			secretClient,
			eventRecorder,
		), 1)
	} else {
		errs = append(errs, fmt.Errorf("missing revisionController; cannot proceed"))
	}

	if len(b.installCommand) > 0 {
		manager.WithController(installer.NewInstallerController(
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
			b.operandNamespace,
			b.staticPodName,
			b.operandName,
			operandInformers,
			b.staticPodOperatorClient,
			configMapClient,
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
			b.certDir,
			b.pruneCommand,
			configMapClient,
			secretClient,
			podClient,
			b.staticPodOperatorClient,
			eventRecorder,
		), 1)
	} else {
		eventRecorder.Warning("PruningControllerMissing", "not enough information provided, not all functionality is present")
	}

	manager.WithController(node.NewNodeController(
		b.staticPodOperatorClient,
		clusterInformers,
		eventRecorder,
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
	).AddKubeInformers(b.kubeInformers), 1)

	manager.WithController(unsupportedconfigoverridescontroller.NewUnsupportedConfigOverridesController(b.staticPodOperatorClient, eventRecorder), 1)
	manager.WithController(loglevel.NewClusterOperatorLoggingController(b.staticPodOperatorClient, eventRecorder), 1)

	return manager, errors.NewAggregate(errs)
}

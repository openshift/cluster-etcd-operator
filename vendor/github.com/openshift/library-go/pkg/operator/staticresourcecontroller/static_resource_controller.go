package staticresourcecontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/restmapper"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/api"
	operatorv1 "github.com/openshift/api/operator/v1"
	migrationv1alpha1 "sigs.k8s.io/kube-storage-version-migrator/pkg/apis/migration/v1alpha1"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

var (
	genericScheme = runtime.NewScheme()
	genericCodecs = serializer.NewCodecFactory(genericScheme)
	genericCodec  = genericCodecs.UniversalDeserializer()
)

func init() {
	utilruntime.Must(api.InstallKube(genericScheme))
	utilruntime.Must(migrationv1alpha1.AddToScheme(genericScheme))
	utilruntime.Must(apiextensions.AddToScheme(genericScheme))
	utilruntime.Must(admissionregistrationv1.AddToScheme(genericScheme))
}

// StaticResourcesPreconditionsFuncType checks if the precondition is met (is true) and then proceeds with the sync.
// When the requirement is not met, the controller reports degraded status.
//
// In case, the returned ready flag is false, a proper error with a valid description is recommended.
type StaticResourcesPreconditionsFuncType func(ctx context.Context) (bool, error)

type StaticResourceController struct {
	name                   string
	manifests              []conditionalManifests
	ignoreNotFoundOnCreate bool
	preconditions          []StaticResourcesPreconditionsFuncType

	operatorClient v1helpers.OperatorClient
	clients        *resourceapply.ClientHolder

	eventRecorder events.Recorder

	factory          *factory.Factory
	restMapper       meta.RESTMapper
	categoryExpander restmapper.CategoryExpander
	performanceCache resourceapply.ResourceCache
}

type conditionalManifests struct {
	// shouldCreateFn controls whether the manifests should be created and updated.
	// If it returns true, the manifests should be applied.
	shouldCreateFn resourceapply.ConditionalFunction
	// shouldDeleteFn controls whether the manifests should be deleted.
	// If it returns true, the manifests should be removed.
	shouldDeleteFn resourceapply.ConditionalFunction

	manifests resourceapply.AssetFunc

	files []string
}

// NewStaticResourceController returns a controller that maintains certain static manifests. Most "normal" types are supported,
// but feel free to add ones we missed.  Use .AddInformer(), .AddKubeInformers(), .AddNamespaceInformer or to provide triggering conditions.
// By default, the controller sets <name>Degraded condition on error when syncing a manifest.
// Optionally, the controller can ignore NotFound errors. This is useful when syncing CRs for CRDs that may not yet exist
// when the controller runs, such as ServiceMonitor.
func NewStaticResourceController(
	name string,
	manifests resourceapply.AssetFunc,
	files []string,
	clients *resourceapply.ClientHolder,
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
) *StaticResourceController {
	c := &StaticResourceController{
		name: name,

		operatorClient: operatorClient,
		clients:        clients,

		preconditions: []StaticResourcesPreconditionsFuncType{defaultStaticResourcesPreconditionsFunc},

		eventRecorder: eventRecorder.WithComponentSuffix(strings.ToLower(name)),

		factory:          factory.New().WithInformers(operatorClient.Informer()).ResyncEvery(1 * time.Minute),
		performanceCache: resourceapply.NewResourceCache(),
	}
	c.WithConditionalResources(manifests, files, nil, nil)

	return c
}

// WithIgnoreNotFoundOnCreate makes the controller to ignore NotFound errors when applying a manifest.
// Such error is returned by the API server when the controller tries to apply a CR for CRD
// that has not yet been created.
// This is useful when creating CRs for other operators that were not started yet (such as ServiceMonitors).
// NotFound errors are reported in <name>Degraded condition, but with Degraded=false.
func (c *StaticResourceController) WithIgnoreNotFoundOnCreate() *StaticResourceController {
	c.ignoreNotFoundOnCreate = true
	return c
}

// WithPrecondition adds a precondition, which blocks the sync method from being executed. Preconditions might be chained using:
//  WithPrecondition(a).WithPrecondition(b).WithPrecondition(c).
// If any of the preconditions is false, the sync will result in an error.
//
// The requirement parameter should follow the convention described in the StaticResourcesPreconditionsFuncType.
//
// When the requirement is not met, the controller reports degraded status.
func (c *StaticResourceController) WithPrecondition(precondition StaticResourcesPreconditionsFuncType) *StaticResourceController {
	c.preconditions = append(c.preconditions, precondition)
	return c
}

// WithConditionalResources adds a set of manifests to be created when the shouldCreateFnArg is true and should be
// deleted when the shouldDeleteFnArg is true.
// If shouldCreateFnArg is nil, then it is always create.
// If shouldDeleteFnArg is nil, then it is !shouldCreateFnArg
//  1. shouldCreateFnArg == true && shouldDeleteFnArg == true - produces an error
//  2. shouldCreateFnArg == false && shouldDeleteFnArg == false - does nothing as expected
//  3. shouldCreateFnArg == true applies the manifests
//  4. shouldDeleteFnArg == true deletes the manifests
func (c *StaticResourceController) WithConditionalResources(manifests resourceapply.AssetFunc, files []string, shouldCreateFnArg, shouldDeleteFnArg resourceapply.ConditionalFunction) *StaticResourceController {
	var shouldCreateFn resourceapply.ConditionalFunction
	if shouldCreateFnArg == nil {
		shouldCreateFn = func() bool {
			return true
		}
	} else {
		shouldCreateFn = shouldCreateFnArg
	}

	var shouldDeleteFn resourceapply.ConditionalFunction
	if shouldDeleteFnArg == nil {
		shouldDeleteFn = func() bool {
			return !shouldCreateFn()
		}
	} else {
		shouldDeleteFn = shouldDeleteFnArg
	}

	c.manifests = append(c.manifests, conditionalManifests{
		shouldCreateFn: shouldCreateFn,
		shouldDeleteFn: shouldDeleteFn,
		manifests:      manifests,
		files:          files,
	})
	return c
}

func (c *StaticResourceController) AddKubeInformers(kubeInformersByNamespace v1helpers.KubeInformersForNamespaces) *StaticResourceController {
	// set the informers so we can have caching clients
	c.clients = c.clients.WithKubernetesInformers(kubeInformersByNamespace)

	ret := c
	for _, conditionalManifest := range c.manifests {
		for _, file := range conditionalManifest.files {
			objBytes, err := conditionalManifest.manifests(file)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("missing %q: %v", file, err))
				continue
			}
			requiredObj, err := resourceread.ReadGenericWithUnstructured(objBytes)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("cannot decode %q: %v", file, err))
				continue
			}
			metadata, err := meta.Accessor(requiredObj)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("cannot get metadata %q: %v", file, err))
				continue
			}

			// find the right subset of informers.  Interestingly, cluster scoped resources require cluster scoped informers
			var informer informers.SharedInformerFactory
			if _, ok := requiredObj.(*corev1.Namespace); ok {
				informer = kubeInformersByNamespace.InformersFor(metadata.GetName())
				if informer == nil {
					utilruntime.HandleError(fmt.Errorf("missing informer for namespace %q; no dynamic wiring added, time-based only.", metadata.GetName()))
					continue
				}
			} else {
				informer = kubeInformersByNamespace.InformersFor(metadata.GetNamespace())
				if informer == nil {
					utilruntime.HandleError(fmt.Errorf("missing informer for namespace %q; no dynamic wiring added, time-based only.", metadata.GetNamespace()))
					continue
				}
			}

			// iterate through the resources we know that are related to kube informers and add the pertinent informers
			switch t := requiredObj.(type) {
			case *corev1.Namespace:
				ret = ret.AddNamespaceInformer(informer.Core().V1().Namespaces().Informer(), t.Name)
			case *corev1.Service:
				ret = ret.AddInformer(informer.Core().V1().Services().Informer())
			case *corev1.Pod:
				ret = ret.AddInformer(informer.Core().V1().Pods().Informer())
			case *corev1.ServiceAccount:
				ret = ret.AddInformer(informer.Core().V1().ServiceAccounts().Informer())
			case *corev1.ConfigMap:
				ret = ret.AddInformer(informer.Core().V1().ConfigMaps().Informer())
			case *corev1.Secret:
				ret = ret.AddInformer(informer.Core().V1().Secrets().Informer())
			case *rbacv1.ClusterRole:
				ret = ret.AddInformer(informer.Rbac().V1().ClusterRoles().Informer())
			case *rbacv1.ClusterRoleBinding:
				ret = ret.AddInformer(informer.Rbac().V1().ClusterRoleBindings().Informer())
			case *rbacv1.Role:
				ret = ret.AddInformer(informer.Rbac().V1().Roles().Informer())
			case *rbacv1.RoleBinding:
				ret = ret.AddInformer(informer.Rbac().V1().RoleBindings().Informer())
			case *policyv1.PodDisruptionBudget:
				ret = ret.AddInformer(informer.Policy().V1().PodDisruptionBudgets().Informer())
			case *storagev1.StorageClass:
				ret = ret.AddInformer(informer.Storage().V1().StorageClasses().Informer())
			case *storagev1.CSIDriver:
				ret = ret.AddInformer(informer.Storage().V1().CSIDrivers().Informer())
			default:
				// if there's a missing case, the caller can add an informer or count on a time based trigger.
				// if the controller doesn't handle it, then there will be failure from the underlying apply.
				klog.V(4).Infof("unhandled type %T", requiredObj)
			}
		}
	}

	return ret
}

func (c *StaticResourceController) AddInformer(informer cache.SharedIndexInformer) *StaticResourceController {
	c.factory.WithInformers(informer)
	return c
}

func (c *StaticResourceController) AddRESTMapper(mapper meta.RESTMapper) *StaticResourceController {
	c.restMapper = mapper
	return c
}

func (c *StaticResourceController) AddCategoryExpander(categoryExpander restmapper.CategoryExpander) *StaticResourceController {
	c.categoryExpander = categoryExpander
	return c
}

func (c *StaticResourceController) AddNamespaceInformer(informer cache.SharedIndexInformer, namespaces ...string) *StaticResourceController {
	c.factory.WithNamespaceInformer(informer, namespaces...)
	return c
}

func (c *StaticResourceController) Sync(ctx context.Context, syncContext factory.SyncContext) error {
	operatorSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	if !management.IsOperatorManaged(operatorSpec.ManagementState) {
		return nil
	}

	for _, precondition := range c.preconditions {
		ready, err := precondition(ctx)
		// We don't care about the other preconditions, we just stop on the first one.
		if !ready {
			var message string
			if err != nil {
				message = err.Error()
			} else {
				message = "the operator didn't specify what preconditions are missing"
			}
			if _, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
				Type:    fmt.Sprintf("%sDegraded", c.name),
				Status:  operatorv1.ConditionTrue,
				Reason:  "PreconditionNotReady",
				Message: message,
			})); updateErr != nil {
				return updateErr
			}
			return err
		}
	}

	errors := []error{}
	var notFoundErrorsCount int
	for _, conditionalManifest := range c.manifests {
		shouldCreate := conditionalManifest.shouldCreateFn()
		shouldDelete := conditionalManifest.shouldDeleteFn()

		var directResourceResults []resourceapply.ApplyResult

		switch {
		case !shouldCreate && !shouldDelete:
			// no action required
			continue
		case shouldCreate && shouldDelete:
			errors = append(errors, fmt.Errorf("cannot create and delete %v at the same time, skipping", strings.Join(conditionalManifest.files, ", ")))
			continue

		case shouldCreate:
			directResourceResults = resourceapply.ApplyDirectly(ctx, c.clients, syncContext.Recorder(), c.performanceCache, conditionalManifest.manifests, conditionalManifest.files...)
		case shouldDelete:
			directResourceResults = resourceapply.DeleteAll(ctx, c.clients, syncContext.Recorder(), conditionalManifest.manifests, conditionalManifest.files...)
		}

		for _, currResult := range directResourceResults {
			if apierrors.IsNotFound(currResult.Error) {
				notFoundErrorsCount++
			}
			if currResult.Error != nil {
				errors = append(errors, fmt.Errorf("%q (%T): %v", currResult.File, currResult.Type, currResult.Error))
				continue
			}
		}
	}

	cnd := operatorv1.OperatorCondition{
		Type:    fmt.Sprintf("%sDegraded", c.name),
		Status:  operatorv1.ConditionFalse,
		Reason:  "AsExpected",
		Message: "",
	}
	if len(errors) > 0 {
		message := ""
		for _, err := range errors {
			message = message + err.Error() + "\n"
		}
		cnd.Status = operatorv1.ConditionTrue
		cnd.Message = message
		cnd.Reason = "SyncError"

		if c.ignoreNotFoundOnCreate && len(errors) == notFoundErrorsCount {
			// all errors were NotFound
			cnd.Status = operatorv1.ConditionFalse
		}
	}

	_, _, err = v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(cnd))
	if err != nil {
		errors = append(errors, err)
	}
	return utilerrors.NewAggregate(errors)
}

func (c *StaticResourceController) Name() string {
	return c.name
}

func (c *StaticResourceController) RelatedObjects() ([]configv1.ObjectReference, error) {
	if c.restMapper == nil {
		return nil, errors.New("StaticResourceController.restMapper is nil")
	}

	if c.categoryExpander == nil {
		return nil, errors.New("StaticResourceController.categoryExpander is nil")
	}

	// create lookup for resources in "all" alias
	grs, _ := c.categoryExpander.Expand("all")
	lookup := make(map[schema.GroupResource]struct{})
	for _, gr := range grs {
		lookup[gr] = struct{}{}
	}

	acc := make([]configv1.ObjectReference, 0)
	errors := []error{}

	for _, conditionalManifest := range c.manifests {
		for _, file := range conditionalManifest.files {
			// parse static asset
			objBytes, err := conditionalManifest.manifests(file)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			requiredObj, err := resourceread.ReadGenericWithUnstructured(objBytes)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			metadata, err := meta.Accessor(requiredObj)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			// map gvk to gvr
			gvk := requiredObj.GetObjectKind().GroupVersionKind()
			mapping, err := c.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil {
				errors = append(errors, err)
				continue
			}
			gvr := mapping.Resource
			// filter out namespaced resources within "all" alias from result
			if metadata.GetNamespace() != "" {
				if _, ok := lookup[gvr.GroupResource()]; ok {
					continue
				}
			}
			acc = append(acc, configv1.ObjectReference{
				Group:     gvk.Group,
				Resource:  gvr.Resource,
				Namespace: metadata.GetNamespace(),
				Name:      metadata.GetName(),
			})
		}
	}

	return acc, utilerrors.NewAggregate(errors)
}

func (c *StaticResourceController) Run(ctx context.Context, workers int) {
	c.factory.WithSync(c.Sync).ToController(c.Name(), c.eventRecorder).Run(ctx, workers)
}

func defaultStaticResourcesPreconditionsFunc(_ context.Context) (bool, error) {
	return true, nil
}

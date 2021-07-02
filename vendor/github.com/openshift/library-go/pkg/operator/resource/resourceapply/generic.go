package resourceapply

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	migrationv1alpha1 "sigs.k8s.io/kube-storage-version-migrator/pkg/apis/migration/v1alpha1"
	migrationclient "sigs.k8s.io/kube-storage-version-migrator/pkg/clients/clientset"

	"github.com/openshift/api"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

var (
	genericScheme = runtime.NewScheme()
	genericCodecs = serializer.NewCodecFactory(genericScheme)
	genericCodec  = genericCodecs.UniversalDeserializer()
)

func init() {
	utilruntime.Must(api.InstallKube(genericScheme))
	utilruntime.Must(apiextensionsv1beta1.AddToScheme(genericScheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(genericScheme))
	utilruntime.Must(migrationv1alpha1.AddToScheme(genericScheme))
	// TODO: remove once openshift/api/pull/929 is merged
	utilruntime.Must(policyv1.AddToScheme(genericScheme))
}

type AssetFunc func(name string) ([]byte, error)

type ApplyResult struct {
	File    string
	Type    string
	Result  runtime.Object
	Changed bool
	Error   error
}

// ConditionalFunction provides needed dependency for a resource on another condition instead of blindly creating
// a resource. This conditional function can also be used to delete the resource when not needed
type ConditionalFunction func() bool

// ResourceConditionalMap maps a file to conditional functions that can be used to create or delete a resource in the
// given file.
type ResourceConditionalMap struct {
	File                  string
	DeleteConditionalFunc ConditionalFunction
	CreateConditionalFunc ConditionalFunction
}

type ClientHolder struct {
	kubeClient          kubernetes.Interface
	apiExtensionsClient apiextensionsclient.Interface
	kubeInformers       v1helpers.KubeInformersForNamespaces
	dynamicClient       dynamic.Interface
	migrationClient     migrationclient.Interface
}

func NewClientHolder() *ClientHolder {
	return &ClientHolder{}
}

func NewKubeClientHolder(client kubernetes.Interface) *ClientHolder {
	return NewClientHolder().WithKubernetes(client)
}

func (c *ClientHolder) WithKubernetes(client kubernetes.Interface) *ClientHolder {
	c.kubeClient = client
	return c
}

func (c *ClientHolder) WithKubernetesInformers(kubeInformers v1helpers.KubeInformersForNamespaces) *ClientHolder {
	c.kubeInformers = kubeInformers
	return c
}

func (c *ClientHolder) WithAPIExtensionsClient(client apiextensionsclient.Interface) *ClientHolder {
	c.apiExtensionsClient = client
	return c
}

func (c *ClientHolder) WithDynamicClient(client dynamic.Interface) *ClientHolder {
	c.dynamicClient = client
	return c
}

func (c *ClientHolder) WithMigrationClient(client migrationclient.Interface) *ClientHolder {
	c.migrationClient = client
	return c
}

func (rcm *ResourceConditionalMap) shouldDelete() bool {
	var shouldDelete bool
	if rcm.CreateConditionalFunc == nil && rcm.DeleteConditionalFunc == nil {
		shouldDelete = false
	} else {
		if rcm.CreateConditionalFunc != nil && rcm.CreateConditionalFunc() {
			shouldDelete = false
		}
		if rcm.CreateConditionalFunc != nil && !rcm.CreateConditionalFunc() {
			shouldDelete = true
		}
		if rcm.DeleteConditionalFunc != nil && rcm.DeleteConditionalFunc() {
			shouldDelete = true
		}
		if rcm.DeleteConditionalFunc == nil && rcm.CreateConditionalFunc() {
			shouldDelete = false
		} else if rcm.DeleteConditionalFunc == nil &&
			!rcm.CreateConditionalFunc() {
			shouldDelete = true
		}
	}
	return shouldDelete
}

// ApplyDirectly applies the given manifest files to API server conditionally.
func ApplyDirectly(clients *ClientHolder, recorder events.Recorder, manifests AssetFunc,
	resourceConditionalMaps []ResourceConditionalMap) []ApplyResult {
	ret := []ApplyResult{}

	for _, resourceConditionalMap := range resourceConditionalMaps {
		result := ApplyResult{File: resourceConditionalMap.File}
		objBytes, err := manifests(resourceConditionalMap.File)
		if err != nil {
			result.Error = fmt.Errorf("missing %q: %v", resourceConditionalMap.File, err)
			ret = append(ret, result)
			continue
		}
		requiredObj, err := decode(objBytes)
		if err != nil {
			result.Error = fmt.Errorf("cannot decode %q: %v", resourceConditionalMap.File, err)
			ret = append(ret, result)
			continue
		}
		// Conditional resource creation or deletion.
		shouldDelete := resourceConditionalMap.shouldDelete()
		result.Type = fmt.Sprintf("%T", requiredObj)
		// NOTE: Do not add CR resources into this switch otherwise the protobuf client can cause problems.
		switch t := requiredObj.(type) {
		case *corev1.Namespace:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyNamespace(clients.kubeClient.CoreV1(), shouldDelete, recorder, t)
			}
		case *corev1.Service:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyService(clients.kubeClient.CoreV1(), shouldDelete, recorder, t)
			}
		case *corev1.Pod:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyPod(clients.kubeClient.CoreV1(), shouldDelete, recorder, t)
			}
		case *corev1.ServiceAccount:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyServiceAccount(clients.kubeClient.CoreV1(), shouldDelete, recorder, t)
			}
		case *corev1.ConfigMap:
			client := clients.configMapsGetter()
			if client == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyConfigMap(client, shouldDelete, recorder, t)
			}
		case *corev1.Secret:
			client := clients.secretsGetter()
			if client == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplySecret(client, shouldDelete, recorder, t)
			}
		case *rbacv1.ClusterRole:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyClusterRole(clients.kubeClient.RbacV1(), shouldDelete, recorder, t)
			}
		case *rbacv1.ClusterRoleBinding:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyClusterRoleBinding(clients.kubeClient.RbacV1(), shouldDelete, recorder, t)
			}
		case *rbacv1.Role:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyRole(clients.kubeClient.RbacV1(), shouldDelete, recorder, t)
			}
		case *rbacv1.RoleBinding:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyRoleBinding(clients.kubeClient.RbacV1(), shouldDelete, recorder, t)
			}
		case *policyv1.PodDisruptionBudget:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyPodDisruptionBudget(clients.kubeClient.PolicyV1(), shouldDelete, recorder, t)
			}
		case *apiextensionsv1beta1.CustomResourceDefinition:
			if clients.apiExtensionsClient == nil {
				result.Error = fmt.Errorf("missing apiExtensionsClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyCustomResourceDefinitionV1Beta1(clients.apiExtensionsClient.ApiextensionsV1beta1(), shouldDelete, recorder, t)
			}
		case *apiextensionsv1.CustomResourceDefinition:
			if clients.apiExtensionsClient == nil {
				result.Error = fmt.Errorf("missing apiExtensionsClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyCustomResourceDefinitionV1(clients.apiExtensionsClient.ApiextensionsV1(), shouldDelete, recorder, t)
			}
		case *storagev1.StorageClass:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyStorageClass(clients.kubeClient.StorageV1(), shouldDelete, recorder, t)
			}
		case *storagev1.CSIDriver:
			if clients.kubeClient == nil {
				result.Error = fmt.Errorf("missing kubeClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyCSIDriver(clients.kubeClient.StorageV1(), shouldDelete, recorder, t)
			}
		case *migrationv1alpha1.StorageVersionMigration:
			if clients.migrationClient == nil {
				result.Error = fmt.Errorf("missing migrationClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyStorageVersionMigration(clients.migrationClient, shouldDelete, recorder, t)
			}
		case *unstructured.Unstructured:
			if clients.dynamicClient == nil {
				result.Error = fmt.Errorf("missing dynamicClient")
			} else {
				result.Result, result.Changed, result.Error = ApplyKnownUnstructured(clients.dynamicClient, shouldDelete, recorder, t)
			}
		default:
			result.Error = fmt.Errorf("unhandled type %T", requiredObj)
		}

		ret = append(ret, result)
	}

	return ret
}

func (c *ClientHolder) configMapsGetter() corev1client.ConfigMapsGetter {
	if c.kubeClient == nil {
		return nil
	}
	if c.kubeInformers == nil {
		return c.kubeClient.CoreV1()
	}
	return v1helpers.CachedConfigMapGetter(c.kubeClient.CoreV1(), c.kubeInformers)
}

func (c *ClientHolder) secretsGetter() corev1client.SecretsGetter {
	if c.kubeClient == nil {
		return nil
	}
	if c.kubeInformers == nil {
		return c.kubeClient.CoreV1()
	}
	return v1helpers.CachedSecretGetter(c.kubeClient.CoreV1(), c.kubeInformers)
}

func decode(objBytes []byte) (runtime.Object, error) {
	// Try to get a typed object first
	typedObj, _, decodeErr := genericCodec.Decode(objBytes, nil, nil)
	if decodeErr == nil {
		return typedObj, nil
	}

	// Try unstructured, hoping to recover from "no kind XXX is registered for version YYY"
	unstructuredObj, _, err := scheme.Codecs.UniversalDecoder().Decode(objBytes, nil, &unstructured.Unstructured{})
	if err != nil {
		// Return the original error
		return nil, decodeErr
	}
	return unstructuredObj, nil
}

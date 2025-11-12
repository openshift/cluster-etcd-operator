package dedicatedetcdcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	opv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"sigs.k8s.io/yaml"
)

// DedicatedEtcdController manages the lifecycle of a dedicated etcd pod for events storage
type DedicatedEtcdController struct {
	staticPodClient    v1helpers.StaticPodOperatorClient
	operatorClient     opv1.OperatorV1Interface
	kubeClient         kubernetes.Interface
	masterNodeLister   corev1listers.NodeLister
	networkLister      configv1listers.NetworkLister
	masterNodeSelector labels.Selector
	etcdImage          string
}

type EventsEtcdDeploymentSubstitution struct {
	Image    string
	NodeName string
}

// NewDedicatedEtcdController creates a new controller for managing dedicated etcd pods
func NewDedicatedEtcdController(
	livenessChecker *health.MultiAlivenessChecker,
	etcdImage string,
	staticPodClient v1helpers.StaticPodOperatorClient,
	kubeClient kubernetes.Interface,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	masterNodeLister corev1listers.NodeLister,
	masterNodeSelector labels.Selector,
	operatorClient opv1.OperatorV1Interface,
	networkLister configv1informers.NetworkInformer,
	eventRecorder events.Recorder) factory.Controller {
	c := &DedicatedEtcdController{
		staticPodClient:    staticPodClient,
		kubeClient:         kubeClient,
		masterNodeLister:   masterNodeLister,
		masterNodeSelector: masterNodeSelector,
		etcdImage:          etcdImage,
		operatorClient:     operatorClient,
		networkLister:      networkLister.Lister(),
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("DedicatedEtcdController", syncer)

	return factory.New().
		ResyncEvery(1*time.Minute).
		WithSync(syncer.Sync).
		WithInformers(
			staticPodClient.Informer(),
			kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer(),
			networkLister.Informer(),
		).
		ToController("DedicatedEtcdController", eventRecorder.WithComponentSuffix("dedicated-etcd-controller"))
}

func (c *DedicatedEtcdController) sync(ctx context.Context, _ factory.SyncContext) error {
	operatorSpec, _, _, err := c.staticPodClient.GetStaticPodOperatorState()
	if err != nil {
		return err
	}

	// Check if the dedicated etcd for events feature is enabled via unsupported override
	enabled, err := ceohelpers.IsDedicatedEtcdForEventsEnabled(operatorSpec)
	if err != nil {
		return fmt.Errorf("failed to check dedicated etcd override: %w", err)
	}

	if !enabled {
		return nil
	}

	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return fmt.Errorf("failed to get cluster network: %w", err)
	}

	controlPlaneNodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return fmt.Errorf("failed to list master nodes: %w", err)
	}

	slices.SortFunc(controlPlaneNodes, func(a, b *corev1.Node) int {
		return strings.Compare(a.Name, b.Name)
	})

	destinedEventNode := controlPlaneNodes[0]
	destinedEventNodeHostname := destinedEventNode.Labels["kubernetes.io/hostname"]
	etcdURLHost, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, destinedEventNode)
	if err != nil {
		return fmt.Errorf("failed to get etcd URL hostname for node %s: %w", destinedEventNode.Name, err)
	}

	depClient := c.kubeClient.AppsV1().Deployments(operatorclient.TargetNamespace)

	renderedDeployment, err := ceohelpers.RenderTemplate("etcd/dedicated-event-etcd-deployment.yaml",
		&EventsEtcdDeploymentSubstitution{Image: c.etcdImage, NodeName: destinedEventNodeHostname})
	if err != nil {
		return fmt.Errorf("failed to render events-etcd deployment: %w", err)
	}

	var deployment appsv1.Deployment
	if err := yaml.Unmarshal([]byte(renderedDeployment), &deployment); err != nil {
		return fmt.Errorf("failed to unmarshal events-etcd deployment: %w", err)
	}

	svcTemplate := bindata.MustAsset("etcd/dedicated-event-etcd-svc.yaml")
	var svc corev1.Service
	if err := yaml.Unmarshal(svcTemplate, &svc); err != nil {
		return fmt.Errorf("failed to unmarshal events-etcd svc: %w", err)
	}

	if _, err = depClient.Get(ctx, deployment.Name, metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			_, err := depClient.Create(ctx, &deployment, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create events-etcd deployment: %w", err)
			}
		} else {
			return fmt.Errorf("failed to get events-etcd deployment: %w", err)
		}
	}

	_, err = depClient.Update(ctx, &deployment, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update events-etcd deployment: %w", err)
	}

	svcClient := c.kubeClient.CoreV1().Services(operatorclient.TargetNamespace)
	if _, err := svcClient.Get(ctx, svc.Name, metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			_, err := svcClient.Create(ctx, &svc, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create events-etcd service: %w", err)
			}
		} else {
			return fmt.Errorf("failed to get events-etcd service: %v", err)
		}
	}

	updatedSvc, err := svcClient.Update(ctx, &svc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update events-etcd service: %w", err)
	}

	kas, err := c.operatorClient.KubeAPIServers().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get kas cluster: %w", err)
	}

	existingUnsupportedConfigOverrides := map[string]interface{}{}
	if len(kas.Spec.UnsupportedConfigOverrides.Raw) > 0 {
		if err := json.NewDecoder(bytes.NewBuffer(kas.Spec.UnsupportedConfigOverrides.Raw)).Decode(&existingUnsupportedConfigOverrides); err != nil {
			return fmt.Errorf("failed to decode existing unsupported config overrides: %w", err)
		}
	}

	/*
		apiVersion: operator.openshift.io/v1
		kind: KubeAPIServer
		spec:
		  unsupportedConfigOverrides:
			apiServerArguments:
			  etcd-servers-overrides:
			  - "/events#https://10.0.0.5:20379"

		  --- OR via SVC ---
			 - "/events#https://events-etcd.openshift-etcd.svc:20379"

			TODO This would require KAS to run with dnsPolicy=ClusterFirstWithHostNet
		https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#pod-s-dns-policy

	*/

	if err := unstructured.SetNestedStringSlice(existingUnsupportedConfigOverrides,
		[]string{fmt.Sprintf("/events#https://%s:%d", etcdURLHost, updatedSvc.Spec.Ports[0].Port)},
		[]string{"apiServerArguments", "etcd-servers-overrides"}...); err != nil {
		return fmt.Errorf("failed to update disabled config-overrides: %w", err)
	}

	kas.Spec.UnsupportedConfigOverrides = runtime.RawExtension{Object: &unstructured.Unstructured{Object: existingUnsupportedConfigOverrides}}
	_, err = c.operatorClient.KubeAPIServers().Update(ctx, kas, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update kas cluster: %w", err)
	}

	// one can verify whether it works by watching the event prefix with etcdctl
	// sh-5.1# etcdctl watch /kubernetes.io/events --prefix
	return nil
}

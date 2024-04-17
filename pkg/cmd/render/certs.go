package render

import (
	"context"
	"fmt"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcdcertsigner"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/resourcesynccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/metrics"
)

// createCertSecrets will run the etcdcertsigner.EtcdCertSignerController once and collect all respective certs created.
// The secrets will contain all signers, peer, serving and client certs. The configmaps contain all bundles.
func createCertSecrets(nodes []*corev1.Node) ([]corev1.Secret, []corev1.ConfigMap, error) {
	var fakeObjs []runtime.Object
	for _, node := range nodes {
		fakeObjs = append(fakeObjs, node)
	}

	fakeKubeClient := fake.NewSimpleClientset(fakeObjs...)
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{
		OperatorSpec: operatorv1.OperatorSpec{
			ManagementState: operatorv1.Managed,
		},
	}, &operatorv1.StaticPodOperatorStatus{
		OperatorStatus: operatorv1.OperatorStatus{Conditions: []operatorv1.OperatorCondition{}},
		NodeStatuses:   []operatorv1.NodeStatus{},
	}, nil, nil)

	kubeInformers := v1helpers.NewKubeInformersForNamespaces(fakeKubeClient, "", "kube-system",
		operatorclient.TargetNamespace, operatorclient.OperatorNamespace, operatorclient.GlobalUserSpecifiedConfigNamespace)
	secretInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets()
	secretLister := secretInformer.Lister()
	secretClient := v1helpers.CachedSecretGetter(fakeKubeClient.CoreV1(), kubeInformers)

	recorder := events.NewInMemoryRecorder("etcd")
	// create openshift-config signers first, they will remain in openshift-config and are needed for the controller sync loop to function
	// TODO(thomas): once the rotation process is in place, we can remove that special case
	etcdSignerCert := tlshelpers.CreateBootstrapSignerCert(secretInformer, secretLister, secretClient, recorder)
	_, _, err := etcdSignerCert.EnsureSigningCertKeyPair(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("could not create etcd signer certificate: %w", err)
	}
	metricsSignerCert := tlshelpers.CreateBootstrapMetricsSignerCert(secretInformer, secretLister, secretClient, recorder)
	_, _, err = metricsSignerCert.EnsureSigningCertKeyPair(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("could not create etcd metrics signer certificate: %w", err)
	}

	nodeSelector, err := labels.Parse("node-role.kubernetes.io/master")
	if err != nil {
		return nil, nil, fmt.Errorf("could not parse master node labels: %w", err)
	}

	controller := etcdcertsigner.NewEtcdCertSignerController(
		health.NewMultiAlivenessChecker(),
		fakeKubeClient,
		fakeOperatorClient,
		kubeInformers,
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		nodeSelector,
		recorder,
		&ceohelpers.AlwaysSafeQuorumChecker{},
		metrics.NewKubeRegistry())

	stopChan := make(chan struct{})
	defer close(stopChan)

	kubeInformers.Start(stopChan)
	for ns := range kubeInformers.Namespaces() {
		kubeInformers.InformersFor(ns).WaitForCacheSync(stopChan)
	}

	err = controller.Sync(context.Background(), factory.NewSyncContext("createCertSecrets", recorder))
	if err != nil {
		return nil, nil, fmt.Errorf("could not run etcd cert signer control loop: %w", err)
	}

	// to finalize, we need to copy a few certificates around, which is handled by the resourcesynccontroller
	syncController, err := resourcesynccontroller.NewResourceSyncController(fakeOperatorClient, kubeInformers, fakeKubeClient, recorder)
	if err != nil {
		return nil, nil, fmt.Errorf("could not create syncController: %w", err)
	}

	err = syncController.Sync(context.Background(), factory.NewSyncContext("createCertSecrets", recorder))
	if err != nil {
		return nil, nil, fmt.Errorf("could not run resourcesynccontroller loop: %w", err)
	}

	openshiftEtcdSecrets, err := fakeKubeClient.CoreV1().Secrets(operatorclient.TargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("error while listing fake client secrets in %s: %w", operatorclient.TargetNamespace, err)
	}

	openshiftConfigSecrets, err := fakeKubeClient.CoreV1().Secrets(operatorclient.GlobalUserSpecifiedConfigNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("error while listing fake client secrets in %s: %w", operatorclient.GlobalUserSpecifiedConfigNamespace, err)
	}

	var secrets []corev1.Secret
	// we have to add some extra information that the fake apiserver doesn't add for a valid k8s resource
	for _, s := range append(openshiftEtcdSecrets.Items, openshiftConfigSecrets.Items...) {
		s.APIVersion = "v1"
		s.Kind = "Secret"
		secrets = append(secrets, s)
	}

	openshiftEtcdBundles, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("error while listing fake client configmaps in %s: %w", operatorclient.TargetNamespace, err)
	}

	openshiftConfigBundles, err := fakeKubeClient.CoreV1().ConfigMaps(operatorclient.GlobalUserSpecifiedConfigNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("error while listing fake client configmaps in %s: %w", operatorclient.GlobalUserSpecifiedConfigNamespace, err)
	}

	var bundles []corev1.ConfigMap
	// we have to add some extra information that the fake apiserver doesn't add for a valid k8s resource
	for _, s := range append(openshiftEtcdBundles.Items, openshiftConfigBundles.Items...) {
		s.APIVersion = "v1"
		s.Kind = "ConfigMap"
		bundles = append(bundles, s)
	}

	return secrets, bundles, nil
}

func createBootstrapCertSecrets(hostName string, ipAddress string) ([]corev1.Secret, []corev1.ConfigMap, error) {
	return createCertSecrets([]*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: hostName, Labels: map[string]string{"node-role.kubernetes.io/master": ""}},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ipAddress}}},
		},
	})
}

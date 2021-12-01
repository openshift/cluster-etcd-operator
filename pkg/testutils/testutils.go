package testutils

import (
	"fmt"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/v3/mock/mockserver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

const (
	clusterConfigName = "cluster-config-v1"
	clusterConfigKey  = "install-config"
)

func MustAbsPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}

func FakeSecret(namespace, name string, cert map[string][]byte) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Data: cert,
	}
	return secret
}

func EndpointsConfigMap(configs ...func(endpoints *corev1.ConfigMap)) *corev1.ConfigMap {
	endpoints := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd-endpoints",
			Namespace: operatorclient.TargetNamespace,
		},
		Data: map[string]string{},
	}
	for _, config := range configs {
		config(endpoints)
	}
	return endpoints
}

func BootstrapConfigMap(configs ...func(bootstrap *corev1.ConfigMap)) *corev1.ConfigMap {
	bootstrap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap",
			Namespace: "kube-system",
		},
		Data: map[string]string{},
	}
	for _, config := range configs {
		config(bootstrap)
	}
	return bootstrap
}

func WithBootstrapStatus(status string) func(*corev1.ConfigMap) {
	return func(bootstrap *corev1.ConfigMap) {
		bootstrap.Data["status"] = status
	}
}

func StaticPodOperatorStatus(configs ...func(status *operatorv1.StaticPodOperatorStatus)) *operatorv1.StaticPodOperatorStatus {
	status := &operatorv1.StaticPodOperatorStatus{
		OperatorStatus: operatorv1.OperatorStatus{
			Conditions: []operatorv1.OperatorCondition{},
		},
		NodeStatuses: []operatorv1.NodeStatus{},
	}
	for _, config := range configs {
		config(status)
	}
	return status
}

func WithBootstrapIP(ip string) func(*corev1.ConfigMap) {
	return func(endpoints *corev1.ConfigMap) {
		if endpoints.Annotations == nil {
			endpoints.Annotations = map[string]string{}
		}
		endpoints.Annotations[etcdcli.BootstrapIPAnnotationKey] = ip
	}
}

func WithEndpoint(memberID uint64, peerURl string) func(*corev1.ConfigMap) {
	if !strings.HasPrefix(peerURl, "https://") {
		peerURl = "https://" + peerURl
	}
	ip, err := dnshelpers.GetIPFromAddress(peerURl)
	if err != nil {
		panic(err)
	}
	return func(endpoints *corev1.ConfigMap) {
		endpoints.Data[fmt.Sprintf("%016x", memberID)] = ip
	}
}

func WithLatestRevision(latest int32) func(status *operatorv1.StaticPodOperatorStatus) {
	return func(status *operatorv1.StaticPodOperatorStatus) {
		status.LatestAvailableRevision = latest
	}
}

func WithNodeStatusAtCurrentRevision(current int32) func(*operatorv1.StaticPodOperatorStatus) {
	return func(status *operatorv1.StaticPodOperatorStatus) {
		status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{
			CurrentRevision: current,
		})
	}
}

func FakeEtcdMember(member int, isLeaner bool, etcdMock []*mockserver.MockServer) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:       fmt.Sprintf("etcd-%d", member),
		ClientURLs: []string{etcdMock[member].Address},
		PeerURLs:   []string{fmt.Sprintf("https://10.0.0.%d:2380", member+1)},
		IsLearner:  isLeaner,
		ID:         FakeMemberId(),
	}
}

func FakeMemberId() uint64 {
	return uint64(rand.Uint32())<<32 + uint64(rand.Uint32())
}

func FakeInfrastructureTopology(topologyMode configv1.TopologyMode) *configv1.Infrastructure {
	return &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: topologyMode,
		},
	}
}

func FakeClusterVersionLister(t *testing.T, clusterVersion *configv1.ClusterVersion) configv1listers.ClusterVersionLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if clusterVersion == nil {
		return configv1listers.NewClusterVersionLister(indexer)
	}

	err := indexer.Add(clusterVersion)
	if err != nil {
		t.Fatal(err)
	}
	return configv1listers.NewClusterVersionLister(indexer)
}

// FakeNetworkLister creates a fake lister.
// !isIpv6  serviceNetwork = []string{"10.0.1.0/24"}
// isIPv6  serviceNetwork = []string{"2001:4860:4860::8888/32"}
func FakeNetworkLister(t *testing.T, isIPv6 bool) configv1listers.NetworkLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	var serviceNetwork []string
	if isIPv6 {
		serviceNetwork = []string{"2001:4860:4860::8888/32"}
	} else {
		serviceNetwork = []string{"10.0.1.0/24"}
	}
	fakeNetworkConfig := &configv1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status:     configv1.NetworkStatus{ServiceNetwork: serviceNetwork},
	}
	if err := indexer.Add(fakeNetworkConfig); err != nil {
		t.Fatal(err.Error())
	}
	return configv1listers.NewNetworkLister(indexer)
}

func GetMockClusterConfig(replicas int) *corev1.ConfigMap {
	installConfig := `apiVersion: v1
controlPlane:
  hyperthreading: Enabled
  name: master`

	if replicas > 0 {
		installConfig = installConfig + fmt.Sprintf("\n  replicas: %d", replicas)
	}

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterConfigName,
			Namespace: operatorclient.TargetNamespace,
		}, Data: map[string]string{clusterConfigKey: installConfig}}
}

// ClusterConfigFullSNO is configmap with embedded install-config and 1 controlPlane replicas.
var ClusterConfigFullSNO = &corev1.ConfigMap{
	TypeMeta: metav1.TypeMeta{},
	ObjectMeta: metav1.ObjectMeta{
		Name:      clusterConfigName,
		Namespace: operatorclient.TargetNamespace,
	}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
  hyperthreading: Enabled
  name: master
  replicas: 1`}}

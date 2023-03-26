package testutils

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/v3/mock/mockserver"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

func MustAbsPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}

func FakePod(name string, configs ...func(node *corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operatorclient.TargetNamespace,
			UID:       uuid.NewUUID(),
		},
	}
	for _, config := range configs {
		config(pod)
	}
	return pod
}

func WithPodStatus(status corev1.PodPhase) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Status = corev1.PodStatus{
			Phase: status,
		}
	}
}

func WithPodLabels(labels map[string]string) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Labels = labels
	}
}

func WithCreationTimestamp(time metav1.Time) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.CreationTimestamp = time
	}
}

func WithScheduledNodeName(name string) func(pod *corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Spec.NodeName = name
	}
}

func FakeNode(name string, configs ...func(node *corev1.Node)) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  uuid.NewUUID(),
		},
	}
	for _, config := range configs {
		config(node)
	}
	return node
}

func WithMasterLabel() func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels["node-role.kubernetes.io/master"] = ""
	}
}

func WithAllocatableStorage(allocatable int64) func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Status.Allocatable == nil {
			node.Status.Allocatable = corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(allocatable, resource.BinarySI),
			}
		}
	}
}

func WithNodeInternalIP(ip string) func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Status.Addresses == nil {
			node.Status.Addresses = []corev1.NodeAddress{}
		}
		node.Status.Addresses = append(node.Status.Addresses, corev1.NodeAddress{
			Type:    corev1.NodeInternalIP,
			Address: ip,
		})
	}
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

func ClusterConfigConfigMap(maxLearner int) *corev1.ConfigMap {
	installConfig := map[string]interface{}{
		"ControlPlane": map[string]interface{}{
			"Replicas": fmt.Sprintf("%d", maxLearner),
		},
	}
	installConfigYaml, _ := yaml.Marshal(installConfig)

	m := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-config-v1",
			Namespace: operatorclient.TargetNamespace,
		},
		Data: map[string]string{
			"install-config": string(installConfigYaml),
		},
	}
	return m
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
		// not relying on the constant from etcdcli.go here as this is introducing a cyclic dependency for its tests
		endpoints.Annotations["alpha.installer.openshift.io/etcd-bootstrap"] = ip
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
		endpoints.Data[fmt.Sprintf("%x", memberID)] = ip
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

func WithNodeStatusAtCurrentRevisionNamed(current int32, name string) func(*operatorv1.StaticPodOperatorStatus) {
	return func(status *operatorv1.StaticPodOperatorStatus) {
		status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{
			NodeName:        name,
			CurrentRevision: current,
		})
	}
}

func FakeEtcdMember(member int, etcdMock []*mockserver.MockServer) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:       fmt.Sprintf("etcd-%d", member),
		ClientURLs: []string{etcdMock[member].Address},
		PeerURLs:   []string{fmt.Sprintf("https://10.0.0.%d:2380", member+1)},
		ID:         fakeMemberId(),
	}
}

func FakeEtcdMemberWithoutServer(member int) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:       fmt.Sprintf("etcd-%d", member),
		ClientURLs: []string{fmt.Sprintf("https://10.0.0.%d:2907", member+1)},
		PeerURLs:   []string{fmt.Sprintf("https://10.0.0.%d:2380", member+1)},
		ID:         uint64(member),
	}
}

func FakeEtcdBoostrapMember(member int) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:       "etcd-bootstrap",
		ClientURLs: []string{fmt.Sprintf("https://10.0.0.%d:2907", member+1)},
		PeerURLs:   []string{fmt.Sprintf("https://10.0.0.%d:2380", member+1)},
		ID:         uint64(member),
	}
}

func AsLearner(member *etcdserverpb.Member) *etcdserverpb.Member {
	member.IsLearner = true
	return member
}

func fakeMemberId() uint64 {
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

func FakeConfigMap(namespace string, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       data,
	}
}

// FakeNetwork creates a fake network.
// !isIpv6  serviceNetwork = []string{"10.0.1.0/24"}
// isIPv6  serviceNetwork = []string{"2001:4860:4860::8888/32"}
func FakeNetwork(isIPv6 bool) *configv1.Network {
	var serviceNetwork []string
	if isIPv6 {
		serviceNetwork = []string{"2001:4860:4860::8888/32"}
	} else {
		serviceNetwork = []string{"10.0.1.0/24"}
	}
	return &configv1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Status:     configv1.NetworkStatus{ServiceNetwork: serviceNetwork},
	}
}

type FakePodLister struct {
	PodList []*corev1.Pod
}

func (f *FakePodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.PodList, nil
}

func (f *FakePodLister) Pods(namespace string) corev1listers.PodNamespaceLister {
	return &fakePodNamespacer{
		Pods: f.PodList,
	}
}

type fakePodNamespacer struct {
	Pods []*corev1.Pod
}

func (f *fakePodNamespacer) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.Pods, nil
}

func (f *fakePodNamespacer) Get(name string) (*corev1.Pod, error) {
	for _, pod := range f.Pods {
		if pod.Name == name {
			return pod, nil
		}
	}
	return nil, errors.New("NotFound")
}

type FakeNodeLister struct {
	Nodes []*corev1.Node
}

func (f *FakeNodeLister) List(selector labels.Selector) ([]*corev1.Node, error) {
	return f.Nodes, nil
}
func (f *FakeNodeLister) Get(name string) (*corev1.Node, error) {
	for _, node := range f.Nodes {
		if node.Name == name {
			return node, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "v1.core.kubernetes.io", Resource: "nodes"}, name)
}

type fakeNodeNamespacer struct {
	Nodes []*corev1.Node
}

func (f *fakeNodeNamespacer) List(selector labels.Selector) ([]*corev1.Node, error) {
	return f.Nodes, nil
}

func (f *fakeNodeNamespacer) Get(name string) (*corev1.Node, error) {
	panic("implement me")
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
	if err := indexer.Add(FakeNetwork(isIPv6)); err != nil {
		t.Fatal(err.Error())
	}
	return configv1listers.NewNetworkLister(indexer)
}

func DefaultEtcdMembers() []*etcdserverpb.Member {
	return []*etcdserverpb.Member{
		FakeEtcdMemberWithoutServer(0),
		FakeEtcdMemberWithoutServer(1),
		FakeEtcdMemberWithoutServer(2),
	}
}

func DefaultInfra() *configv1.Infrastructure {
	return &configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
	}
}

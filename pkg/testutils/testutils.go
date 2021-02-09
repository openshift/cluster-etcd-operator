package testutils

import (
	"encoding/base64"
	"fmt"

	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/mock/mockserver"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

func FakeNodeWithUID(name string, uid types.UID, configs ...func(node *corev1.Node)) *corev1.Node {
	node := FakeNode(name, configs...)
	node.UID = uid
	return node
}

func FakeNode(name string, configs ...func(node *corev1.Node)) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
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

func WithAddress(ip string) func(*corev1.ConfigMap) {
	return func(endpoints *corev1.ConfigMap) {
		endpoints.Data[base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(ip))] = ip
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

func FakeEtcdMember(member int, etcdMock []*mockserver.MockServer) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:       fmt.Sprintf("etcd-%d", member),
		ClientURLs: []string{etcdMock[member].Address},
	}
}

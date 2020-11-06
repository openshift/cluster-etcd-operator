package etcdendpointscontroller

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/mock/mockserver"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/diff"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func TestBootstrapAnnotationRemoval(t *testing.T) {
	mockEtcd, err := mockserver.StartMockServers(3)
	if err != nil {
		t.Fatalf("failed to start mock servers: %s", err)
	}
	defer mockEtcd.Stop()
	scenarios := []struct {
		name            string
		objects         []runtime.Object
		staticPodStatus *operatorv1.StaticPodOperatorStatus
		etcdMembers     []*etcdserverpb.Member
		validateFunc    func(ts *testing.T, actions []clientgotesting.Action)
	}{
		{
			// The bootstrap IP should be deleted because bootstrap reports complete
			// and all nodes have converged on a revision.
			name: "NewClusterBootstrapRemoval",
			objects: []runtime.Object{
				node("master-0", withMasterLabel(), withNodeInternalIP("10.0.0.1")),
				node("master-1", withMasterLabel(), withNodeInternalIP("10.0.0.2")),
				node("master-2", withMasterLabel(), withNodeInternalIP("10.0.0.3")),
				bootstrapConfigMap(withBootstrapStatus("complete")),
				endpointsConfigMap(
					withBootstrapIP("192.0.2.1"),
					withAddress("10.0.0.1"),
					withAddress("10.0.0.2"),
					withAddress("10.0.0.3"),
				),
			},
			staticPodStatus: staticPodOperatorStatus(
				withLatestRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				etcdMember(0, mockEtcd.Servers),
				etcdMember(1, mockEtcd.Servers),
				etcdMember(2, mockEtcd.Servers),
			},
			validateFunc: func(ts *testing.T, actions []clientgotesting.Action) {
				wasValidated := false
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						expected := endpointsConfigMap(
							withAddress("10.0.0.1"),
							withAddress("10.0.0.2"),
							withAddress("10.0.0.3"),
						)
						if !equality.Semantic.DeepEqual(actual, expected) {
							ts.Errorf(diff.ObjectDiff(expected, actual))
						}
						wasValidated = true
						break
					}
				}
				if !wasValidated {
					ts.Errorf("the endpoints configmap wasn't validated")
				}
			},
		},
		{
			// The configmap should remain intact because although bootstrapping
			// reports complete, the nodes are still progressing towards a revision.
			name: "NewClusterBootstrapNodesProgressing",
			objects: []runtime.Object{
				node("master-0", withMasterLabel(), withNodeInternalIP("10.0.0.1")),
				node("master-1", withMasterLabel(), withNodeInternalIP("10.0.0.2")),
				node("master-2", withMasterLabel(), withNodeInternalIP("10.0.0.3")),
				bootstrapConfigMap(withBootstrapStatus("complete")),
				endpointsConfigMap(
					withBootstrapIP("192.0.2.1"),
					withAddress("10.0.0.1"),
					withAddress("10.0.0.2"),
					withAddress("10.0.0.3"),
				),
			},
			staticPodStatus: staticPodOperatorStatus(
				withLatestRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(2),
				withNodeStatusAtCurrentRevision(3),
			),
			validateFunc: func(ts *testing.T, actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						ts.Errorf("unexpected configmap update: %#v", actual)
					}
				}
			},
		},
		{
			// The configmap should remain intact because although nodes appear
			// to have converged on a revision, bootstrap reports incomplete.
			name: "NewClusterBootstrapProgressing",
			objects: []runtime.Object{
				node("master-0", withMasterLabel(), withNodeInternalIP("10.0.0.1")),
				node("master-1", withMasterLabel(), withNodeInternalIP("10.0.0.2")),
				node("master-2", withMasterLabel(), withNodeInternalIP("10.0.0.3")),
				bootstrapConfigMap(withBootstrapStatus("progressing")),
				endpointsConfigMap(
					withBootstrapIP("192.0.2.1"),
					withAddress("10.0.0.1"),
					withAddress("10.0.0.2"),
					withAddress("10.0.0.3"),
				),
			},
			staticPodStatus: staticPodOperatorStatus(
				withLatestRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
			),
			validateFunc: func(ts *testing.T, actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						ts.Errorf("unexpected configmap update: %#v", actual)
					}
				}
			},
		},
		{
			// The configmap should remain intact because there are no changes.
			name: "NewClusterSteadyState",
			objects: []runtime.Object{
				node("master-0", withMasterLabel(), withNodeInternalIP("10.0.0.1")),
				node("master-1", withMasterLabel(), withNodeInternalIP("10.0.0.2")),
				node("master-2", withMasterLabel(), withNodeInternalIP("10.0.0.3")),
				bootstrapConfigMap(withBootstrapStatus("complete")),
				endpointsConfigMap(
					withAddress("10.0.0.1"),
					withAddress("10.0.0.2"),
					withAddress("10.0.0.3"),
				),
			},
			staticPodStatus: staticPodOperatorStatus(
				withLatestRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
			),
			validateFunc: func(ts *testing.T, actions []clientgotesting.Action) {
				for _, action := range actions {
					if action.Matches("update", "configmaps") {
						updateAction := action.(clientgotesting.UpdateAction)
						actual := updateAction.GetObject().(*corev1.ConfigMap)
						ts.Errorf("unexpected configmap update: %#v", actual)
					}
				}
			},
		},
		{
			// The configmap should be created without a bootstrap IP because the
			// only time the configmap won't already exist is when we've upgraded
			// from a pre-configmap cluster (in which case bootstrapping already
			// happened) or someone has deleted the configmap (which also implies
			// bootstrap already happened). An edge case not specifically accounted
			// for is the configmap being deleted before bootstrapping is complete.
			name: "UpgradedClusterCreateConfigmap",
			objects: []runtime.Object{
				node("master-0", withMasterLabel(), withNodeInternalIP("10.0.0.1")),
				node("master-1", withMasterLabel(), withNodeInternalIP("10.0.0.2")),
				node("master-2", withMasterLabel(), withNodeInternalIP("10.0.0.3")),
				bootstrapConfigMap(withBootstrapStatus("complete")),
			},
			staticPodStatus: staticPodOperatorStatus(
				withLatestRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
				withNodeStatusAtCurrentRevision(3),
			),
			validateFunc: func(ts *testing.T, actions []clientgotesting.Action) {
				wasValidated := false
				for _, action := range actions {
					if action.Matches("create", "configmaps") {
						createAction := action.(clientgotesting.CreateAction)
						actual := createAction.GetObject().(*corev1.ConfigMap)
						expected := endpointsConfigMap(
							withAddress("10.0.0.1"),
							withAddress("10.0.0.2"),
							withAddress("10.0.0.3"),
						)
						if !equality.Semantic.DeepEqual(actual, expected) {
							ts.Errorf(diff.ObjectDiff(expected, actual))
						}
						wasValidated = true
						break
					}
				}
				if !wasValidated {
					ts.Errorf("the endpoints configmap wasn't validated")
				}
			},
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				scenario.staticPodStatus,
				nil,
				nil,
			)

			fakeKubeClient := fake.NewSimpleClientset(scenario.objects...)
			fakeEtcdClient := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace), "test-etcdendpointscontroller", &corev1.ObjectReference{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			controller := &EtcdEndpointsController{
				operatorClient:  fakeOperatorClient,
				etcdClient:      fakeEtcdClient,
				nodeLister:      corev1listers.NewNodeLister(indexer),
				configmapLister: corev1listers.NewConfigMapLister(indexer),
				configmapClient: fakeKubeClient.CoreV1(),
			}

			err := controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			if err != nil {
				t.Fatal(err)
			}
			if scenario.validateFunc != nil {
				scenario.validateFunc(t, fakeKubeClient.Actions())
			}
		})
	}
}

func bootstrapConfigMap(configs ...func(bootstrap *corev1.ConfigMap)) *corev1.ConfigMap {
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

func withBootstrapStatus(status string) func(*corev1.ConfigMap) {
	return func(bootstrap *corev1.ConfigMap) {
		bootstrap.Data["status"] = status
	}
}

func staticPodOperatorStatus(configs ...func(status *operatorv1.StaticPodOperatorStatus)) *operatorv1.StaticPodOperatorStatus {
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

func withLatestRevision(latest int32) func(status *operatorv1.StaticPodOperatorStatus) {
	return func(status *operatorv1.StaticPodOperatorStatus) {
		status.LatestAvailableRevision = latest
	}
}

func withNodeStatusAtCurrentRevision(current int32) func(*operatorv1.StaticPodOperatorStatus) {
	return func(status *operatorv1.StaticPodOperatorStatus) {
		status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{
			CurrentRevision: current,
		})
	}
}

func endpointsConfigMap(configs ...func(endpoints *corev1.ConfigMap)) *corev1.ConfigMap {
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

func withBootstrapIP(ip string) func(*corev1.ConfigMap) {
	return func(endpoints *corev1.ConfigMap) {
		if endpoints.Annotations == nil {
			endpoints.Annotations = map[string]string{}
		}
		endpoints.Annotations[etcdcli.BootstrapIPAnnotationKey] = ip
	}
}

func withAddress(ip string) func(*corev1.ConfigMap) {
	return func(endpoints *corev1.ConfigMap) {
		endpoints.Data[base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(ip))] = ip
	}
}

func node(name string, configs ...func(node *corev1.Node)) *corev1.Node {
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

func withMasterLabel() func(*corev1.Node) {
	return func(node *corev1.Node) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels["node-role.kubernetes.io/master"] = ""
	}
}

func withNodeInternalIP(ip string) func(*corev1.Node) {
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

func etcdMember(member int, etcdMock []*mockserver.MockServer) *etcdserverpb.Member {
	return &etcdserverpb.Member{
		Name:       fmt.Sprintf("etcd-%d", member),
		ClientURLs: []string{etcdMock[member].Address},
	}
}

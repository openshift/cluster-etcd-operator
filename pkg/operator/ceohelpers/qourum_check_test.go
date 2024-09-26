package ceohelpers

import (
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/openshift/client-go/config/informers/externalversions"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	machinelistersv1beta1 "github.com/openshift/client-go/machine/listers/machine/v1beta1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

func TestQuorumCheck_IsSafeToUpdateRevision(t *testing.T) {

	defaultEtcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
	}

	// this is largely the same as in boostrap_test.go
	defaultObjects := []runtime.Object{
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
		},
		&configv1.Infrastructure{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name: InfrastructureClusterName,
			},
			Status: configv1.InfrastructureStatus{
				ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
		},
	}

	scenarios := []struct {
		name            string
		objects         []runtime.Object
		staticPodStatus *operatorv1.StaticPodOperatorStatus
		etcdMembers     []*etcdserverpb.Member
		endpointsString string

		safe        bool
		expectedErr error
	}{
		{
			name: "HappyPath",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: defaultEtcdMembers,
			safe:        true,
		},
		{
			name: "Incomplete Quorum during bootstrap",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("in progress")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
			},
			safe: true,
		},
		{
			name: "Incomplete Quorum",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
			},
			safe:        false,
			expectedErr: fmt.Errorf("etcd cluster has quorum of 2 which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:1 name:\"etcd-1\" peerURLs:\"https://10.0.0.2:2380\" clientURLs:\"https://10.0.0.2:2907\"  Healthy:true Took: Error:<nil>}]"),
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

			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range defaultObjects {
				require.NoError(t, indexer.Add(obj))
			}

			for _, obj := range scenario.objects {
				require.NoError(t, indexer.Add(obj))
			}

			fakeMachineAPIChecker := &fakeMachineAPI{isMachineAPIFunctional: func() (bool, error) { return false, nil }}
			machineLister := machinelistersv1beta1.NewMachineLister(indexer)
			machineSelector, err := labels.Parse("machine.openshift.io/cluster-api-machine-role=master")
			if err != nil {
				t.Fatal(err)
			}

			nodeLister := corev1listers.NewNodeLister(indexer)

			fakeClient := fake.NewSimpleClientset()
			informerFactory := externalversions.NewSharedInformerFactory(fakeClient, 0)
			networkInformer := informerFactory.Config().V1().Networks()

			quorumChecker := NewQuorumChecker(
				corev1listers.NewConfigMapLister(indexer),
				corev1listers.NewNamespaceLister(indexer),
				configv1listers.NewInfrastructureLister(indexer),
				fakeOperatorClient,
				fakeEtcdClient,
				fakeMachineAPIChecker,
				machineLister,
				machineSelector,
				nodeLister,
				networkInformer,
			)

			safe, err := quorumChecker.IsSafeToUpdateRevision()
			assert.Equal(t, scenario.expectedErr, err)
			assert.Equal(t, scenario.safe, safe)
		})
	}
}

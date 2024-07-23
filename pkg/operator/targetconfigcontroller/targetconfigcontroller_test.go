package targetconfigcontroller

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/config/v1alpha1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"go.etcd.io/etcd/api/v3/etcdserverpb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTargetConfigController(t *testing.T) {

	scenarios := []struct {
		name              string
		objects           []runtime.Object
		staticPodStatus   *operatorv1.StaticPodOperatorStatus
		etcdMembers       []*etcdserverpb.Member
		etcdMembersEnvVar string
		expectedErr       error
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
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			etcdMembersEnvVar: "1,2,3",
		},
		{
			name: "Quorum not fault tolerant",
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
				u.FakeEtcdMemberWithoutServer(2),
			},
			etcdMembersEnvVar: "1,3",
			expectedErr:       fmt.Errorf("TargetConfigController can't evaluate whether quorum is safe: %w", fmt.Errorf("etcd cluster has quorum of 2 which is not fault tolerant: [{Member:name:\"etcd-0\" peerURLs:\"https://10.0.0.1:2380\" clientURLs:\"https://10.0.0.1:2907\"  Healthy:true Took: Error:<nil>} {Member:ID:2 name:\"etcd-2\" peerURLs:\"https://10.0.0.3:2380\" clientURLs:\"https://10.0.0.3:2907\"  Healthy:true Took: Error:<nil>}]")),
		},
		{
			name: "Quorum not fault tolerant but bootstrapping",
			objects: []runtime.Object{
				u.BootstrapConfigMap(u.WithBootstrapStatus("not complete")),
			},
			staticPodStatus: u.StaticPodOperatorStatus(
				u.WithLatestRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
				u.WithNodeStatusAtCurrentRevision(3),
			),
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(0),
				u.FakeEtcdMemberWithoutServer(2),
			},
			etcdMembersEnvVar: "1,3",
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
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(scenario.etcdMembers)
			if err != nil {
				t.Fatal(err)
			}

			defaultObjects := []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: operatorclient.TargetNamespace},
				},
				&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: ceohelpers.InfrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
				},
			}

			eventRecorder := events.NewRecorder(fakeKubeClient.CoreV1().Events(operatorclient.TargetNamespace),
				"test-targetconfigcontroller", &corev1.ObjectReference{})
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range defaultObjects {
				require.NoError(t, indexer.Add(obj))
			}
			for _, obj := range scenario.objects {
				require.NoError(t, indexer.Add(obj))
			}

			envVar := etcdenvvar.FakeEnvVar{EnvVars: map[string]string{
				"ALL_ETCD_ENDPOINTS": scenario.etcdMembersEnvVar,
			}}

			quorumChecker := ceohelpers.NewQuorumChecker(
				corev1listers.NewConfigMapLister(indexer),
				corev1listers.NewNamespaceLister(indexer),
				configv1listers.NewInfrastructureLister(indexer),
				fakeOperatorClient,
				fakeEtcdClient)

			controller := &TargetConfigController{
				targetImagePullSpec:   "etcd-pull-spec",
				operatorImagePullSpec: "operator-pull-spec",
				operatorClient:        fakeOperatorClient,
				kubeClient:            fakeKubeClient,
				envVarGetter:          envVar,
				backupVarGetter:       backuphelpers.NewDisabledBackupConfig(v1alpha1.EtcdBackupSpec{}),
				enqueueFn:             func() {},
				quorumChecker:         quorumChecker,
			}

			err = controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			assert.Equal(t, scenario.expectedErr, err)
		})
	}
}

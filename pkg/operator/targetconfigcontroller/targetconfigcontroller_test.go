package targetconfigcontroller

import (
	"context"
	"fmt"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
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
			eventRecorder, _, controller := getController(t, scenario.staticPodStatus, scenario.objects, scenario.etcdMembers)
			err := controller.sync(context.TODO(), factory.NewSyncContext("test", eventRecorder))
			assert.Equal(t, scenario.expectedErr, err)
		})
	}
}

func TestControllerDegradesOnQuorumLoss(t *testing.T) {
	objects := []runtime.Object{
		u.BootstrapConfigMap(u.WithBootstrapStatus("complete")),
	}

	staticPodStatus := u.StaticPodOperatorStatus(
		u.WithLatestRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
		u.WithNodeStatusAtCurrentRevision(3),
	)
	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(0),
		u.FakeEtcdMemberWithoutServer(2),
	}

	eventRecorder, fakeOperatorClient, controller := getController(t, staticPodStatus, objects, etcdMembers)
	syncContext := factory.NewSyncContext("test", eventRecorder)

	foundDegraded := false
	for i := 0; i < 100; i++ {
		require.Error(t, controller.sync(context.TODO(), syncContext))
		// check that the controller eventually degrades
		_, status, _, err := fakeOperatorClient.GetOperatorState()
		require.NoError(t, err)

		// we only expect one condition to be set here
		if len(status.Conditions) > 0 {
			require.Equal(t, "TargetConfigControllerDegraded", status.Conditions[0].Type)
			require.Equal(t, "SynchronizationError", status.Conditions[0].Reason)
			require.Contains(t, status.Conditions[0].Message, "TargetConfigController can't evaluate whether quorum is safe: etcd cluster has quorum of 2 which is not fault tolerant")
			foundDegraded = true
			break
		}
	}

	require.Truef(t, foundDegraded, "could not find degraded status in operator client")
}

func getController(t *testing.T, staticPodStatus *operatorv1.StaticPodOperatorStatus, objects []runtime.Object, etcdMembers []*etcdserverpb.Member) (events.Recorder, v1helpers.StaticPodOperatorClient, *TargetConfigController) {
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{
			OperatorSpec: operatorv1.OperatorSpec{
				ManagementState: operatorv1.Managed,
			},
		},
		staticPodStatus,
		nil,
		nil,
	)

	fakeKubeClient := fake.NewSimpleClientset(objects...)
	fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(etcdMembers)
	require.NoError(t, err)

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
	for _, obj := range objects {
		require.NoError(t, indexer.Add(obj))
	}

	envVar := etcdenvvar.FakeEnvVar{EnvVars: map[string]string{
		"ALL_ETCD_ENDPOINTS": "1,3",
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
		backupVarGetter:       backuphelpers.NewDisabledBackupConfig(),
		enqueueFn:             func() {},
		quorumChecker:         quorumChecker,
	}

	return eventRecorder, fakeOperatorClient, controller
}

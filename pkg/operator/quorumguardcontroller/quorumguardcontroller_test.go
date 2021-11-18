package quorumguardcontroller

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakecore "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

func TestQuorumGuardController_ensureEtcdGuard(t *testing.T) {
	clusterConfigFullHA := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterConfigName,
			Namespace: operatorclient.TargetNamespace,
		}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
  hyperthreading: Enabled
  name: master
  replicas: 3`}}

	pdb := getPDB(1)
	changedPDB := getPDB(1)
	changedPDB.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-changed": EtcdGuardDeploymentName}}
	deployment := resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	changedDeployment := resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	changedDeployment.Spec.Replicas = pointer.Int32Ptr(1)

	haInfra := configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: infrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
	}

	type fields struct {
		client           kubernetes.Interface
		infraObj         configv1.Infrastructure
		clusterConfigObj corev1.ConfigMap
	}
	tests := []struct {
		name                 string
		fields               fields
		wantErr              bool
		expectedHATopology   configv1.TopologyMode
		expectedEvents       int
		expectedReplicaCount int
	}{
		{
			name: "test ensureEtcdGuard - deployment exists but pdb not",
			fields: fields{
				client:           fakecore.NewSimpleClientset(deployment),
				infraObj:         haInfra,
				clusterConfigObj: clusterConfigFullHA,
			},

			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       2,
			wantErr:              false,
			expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment and pdb exists",
			fields: fields{
				client:           fakecore.NewSimpleClientset(deployment, pdb),
				infraObj:         haInfra,
				clusterConfigObj: clusterConfigFullHA,
			},
			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       1,
			wantErr:              false,
			expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment not exists but pdb exists",
			fields: fields{
				client:           fakecore.NewSimpleClientset(pdb),
				infraObj:         haInfra,
				clusterConfigObj: clusterConfigFullHA,
			},
			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       1,
			wantErr:              false,
			expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment was changed and pdb exists",
			fields: fields{
				client:           fakecore.NewSimpleClientset(changedDeployment, pdb),
				infraObj:         haInfra,
				clusterConfigObj: clusterConfigFullHA,
			},
			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       1,
			wantErr:              false,
			expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment exists and pdb was changed",
			fields: fields{
				client:           fakecore.NewSimpleClientset(deployment, changedPDB),
				infraObj:         haInfra,
				clusterConfigObj: clusterConfigFullHA,
			},
			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       2,
			wantErr:              false,
			expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment and pdb were changed",
			fields: fields{
				client:           fakecore.NewSimpleClientset(changedDeployment, changedPDB),
				infraObj:         haInfra,
				clusterConfigObj: clusterConfigFullHA,
			},
			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       2,
			wantErr:              false,
			expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - non HA mode",
			fields: fields{
				client: fakecore.NewSimpleClientset(),
				infraObj: configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						ControlPlaneTopology: configv1.SingleReplicaTopologyMode},
				}},
			expectedHATopology:   configv1.SingleReplicaTopologyMode,
			expectedEvents:       0,
			wantErr:              false,
			expectedReplicaCount: 0,
		},
		{
			name: "test ensureEtcdGuard - HA mode not set, nothing exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(),
				infraObj: configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{}}},
			expectedHATopology:   "",
			expectedEvents:       0,
			wantErr:              true,
			expectedReplicaCount: 0,
		},
		{
			name: "test ensureEtcdGuard - 5 replicas and nothing exists",
			fields: fields{
				client:   fakecore.NewSimpleClientset(),
				infraObj: haInfra,
				clusterConfigObj: corev1.ConfigMap{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterConfigName,
						Namespace: operatorclient.TargetNamespace,
					}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
 hyperthreading: Enabled
 name: master
 replicas: 5`}},
			},
			expectedHATopology:   configv1.HighlyAvailableTopologyMode,
			expectedEvents:       2,
			wantErr:              false,
			expectedReplicaCount: 5,
		},
		{
			name: "test ensureEtcdGuard - get clusterConfig not exists",
			fields: fields{
				client:   fakecore.NewSimpleClientset(),
				infraObj: haInfra,
			},
			expectedHATopology: configv1.HighlyAvailableTopologyMode,
			expectedEvents:     0,
			wantErr:            true,
		},
		{
			name: "test ensureEtcdGuard - get replicas count key not found",
			fields: fields{
				client:   fakecore.NewSimpleClientset(),
				infraObj: haInfra,
				clusterConfigObj: corev1.ConfigMap{TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterConfigName,
						Namespace: operatorclient.TargetNamespace,
					}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
 hyperthreading: Enabled
 name: master`}},
			},
			expectedHATopology: configv1.HighlyAvailableTopologyMode,
			expectedEvents:     0,
			wantErr:            true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := events.NewInMemoryRecorder("test")

			fakeInfraIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if err := fakeInfraIndexer.Add(&tt.fields.infraObj); err != nil {
				t.Fatal(err)
			}

			fakeConfigMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			if err := fakeConfigMapIndexer.Add(&tt.fields.clusterConfigObj); err != nil {
				t.Fatal(err)
			}

			c := &QuorumGuardController{
				kubeClient:           tt.fields.client,
				infrastructureLister: configv1listers.NewInfrastructureLister(fakeInfraIndexer),
				configMapLister:      corev1listers.NewConfigMapLister(fakeConfigMapIndexer),
			}
			err := c.ensureEtcdGuard(context.TODO(), recorder)
			if (err != nil) != tt.wantErr {
				t.Errorf("ensureEtcdGuard() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(recorder.Events()) != tt.expectedEvents {
				t.Errorf("number of events %d and expected %d", len(recorder.Events()), tt.expectedEvents)
				return
			}

			if c.clusterTopology != tt.expectedHATopology {
				t.Errorf("cluster HA topology is %q and expected %q", c.clusterTopology, tt.expectedHATopology)
				return
			}

			if c.desiredControlPlaneSize != tt.expectedReplicaCount {
				t.Errorf("desiredControlPlaneSize is %d and expected is %d", c.desiredControlPlaneSize, tt.expectedReplicaCount)
				return
			}
		})
	}
}

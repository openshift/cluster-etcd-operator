package quorumguardcontroller

import (
	"context"
	"k8s.io/utils/pointer"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakecore "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/etcd_assets"
)

func TestQuorumGuardController_ensureEtcdGuard(t *testing.T) {
	clusterConfigFullHA := corev1.ConfigMap{TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterConfigName,
			Namespace: clusterConfigNamespace,
		}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
  hyperthreading: Enabled
  name: master
  replicas: 3`}}

	changedPDB := &policyv1beta1.PodDisruptionBudget{}
	*changedPDB = *pdb
	changedPDB.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-changed": EtcdGuardDeploymentName}}
	deployment := resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	changedDeployment := resourceread.ReadDeploymentV1OrDie(etcd_assets.MustAsset("etcd/quorumguard-deployment.yaml"))
	changedDeployment.Spec.Replicas = pointer.Int32Ptr(1)

	fakeInfraIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	haInfra := &configv1.Infrastructure{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: infrastructureClusterName,
		},
		Status: configv1.InfrastructureStatus{
			ControlPlaneTopology: configv1.HighlyAvailableTopologyMode},
	}

	type fields struct {
		client   kubernetes.Interface
		infraObj *configv1.Infrastructure
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
			name: "test ensureEtcdGuard - deployment exists but pdb not ",
			fields: fields{
				client:   fakecore.NewSimpleClientset(deployment, &clusterConfigFullHA),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 1, wantErr: false, expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment and pdb exists",
			fields: fields{
				client:   fakecore.NewSimpleClientset(deployment, pdb, &clusterConfigFullHA),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 0, wantErr: false, expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment not exists but pdb exists",
			fields: fields{
				client:   fakecore.NewSimpleClientset(&clusterConfigFullHA, pdb),
				infraObj: haInfra,
			},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 1, wantErr: false, expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment was changed and pdb exists",
			fields: fields{
				client:   fakecore.NewSimpleClientset(changedDeployment, pdb, &clusterConfigFullHA),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 1, wantErr: false, expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment exists and pdb was changed",
			fields: fields{
				client:   fakecore.NewSimpleClientset(deployment, changedPDB, &clusterConfigFullHA),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 1, wantErr: false, expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - deployment and pdb were changed",
			fields: fields{
				client:   fakecore.NewSimpleClientset(changedDeployment, changedPDB, &clusterConfigFullHA),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 2, wantErr: false, expectedReplicaCount: 3,
		},
		{
			name: "test ensureEtcdGuard - nonHAmod",
			fields: fields{
				client: fakecore.NewSimpleClientset(),
				infraObj: &configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						ControlPlaneTopology: configv1.SingleReplicaTopologyMode},
				}},
			expectedHATopology: configv1.SingleReplicaTopologyMode, expectedEvents: 0, wantErr: false, expectedReplicaCount: 0,
		},
		{
			name: "test ensureEtcdGuard - ha mod not set, nothing exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(&clusterConfigFullHA),
				infraObj: &configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{}}},
			expectedHATopology: "", expectedEvents: 0, wantErr: true, expectedReplicaCount: 0,
		},
		{
			name: "test ensureEtcdGuard - 5 replicas and nothing exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterConfigName,
						Namespace: clusterConfigNamespace,
					}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
 hyperthreading: Enabled
 name: master
 replicas: 5`}}),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 2, wantErr: false, expectedReplicaCount: 5,
		},
		{
			name: "test ensureEtcdGuard - get clusterConfig not exists",
			fields: fields{
				client:   fakecore.NewSimpleClientset(),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 0, wantErr: true,
		},
		{
			name: "test ensureEtcdGuard - get replicas count key not found",
			fields: fields{
				client: fakecore.NewSimpleClientset(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterConfigName,
						Namespace: clusterConfigNamespace,
					}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
 hyperthreading: Enabled
 name: master`}}),
				infraObj: haInfra},
			expectedHATopology: configv1.HighlyAvailableTopologyMode, expectedEvents: 0, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := events.NewInMemoryRecorder("test")

			if err := fakeInfraIndexer.Add(tt.fields.infraObj); err != nil {
				t.Fatal(err)
			}

			c := &QuorumGuardController{
				kubeClient:           tt.fields.client,
				infrastructureLister: configv1listers.NewInfrastructureLister(fakeInfraIndexer),
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

			if c.replicaCount != tt.expectedReplicaCount {
				t.Errorf("replicaCount is %d and expected is %d", c.replicaCount, tt.expectedReplicaCount)
				return
			}
		})
	}
}

package quorumguardcontroller

import (
	"context"
	configv1 "github.com/openshift/api/config/v1"
	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	configclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	fakecore "k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestQuorumGuardController_ensureEtcdGuardDeployment(t *testing.T) {
	clusterConfigFullHA := corev1.ConfigMap{TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterConfigName,
			Namespace: clusterConfigNamespace,
		}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
  hyperthreading: Enabled
  name: master
  replicas: 3`}}

	pdb := policyv1beta1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      EtcdGuardDeploymentName,
			Namespace: operatorclient.TargetNamespace,
		},
	}

	type fields struct {
		client    kubernetes.Interface
		clientInf configclientv1.InfrastructuresGetter
	}
	tests := []struct {
		name                 string
		fields               fields
		wantErr              bool
		expectedHAmode       configv1.HighAvailabilityMode
		expectedEvents       int
		expectedReplicaCount int
	}{
		{
			name: "test ensureEtcdGuardDeployment - deployment exists but pdb not ",
			fields: fields{
				client: fakecore.NewSimpleClientset(&appsv1.Deployment{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      EtcdGuardDeploymentName,
						Namespace: operatorclient.TargetNamespace,
					},
					Spec:   appsv1.DeploymentSpec{},
					Status: appsv1.DeploymentStatus{},
				}, &clusterConfigFullHA),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						HighAvailabilityMode: configv1.FullHighAvailabilityMode},
				}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 2, wantErr: false, expectedReplicaCount: 3,
		},

		{
			name: "test ensureEtcdGuardDeployment - deployment and pdb exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(&appsv1.Deployment{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      EtcdGuardDeploymentName,
						Namespace: operatorclient.TargetNamespace,
					},
					Spec:   appsv1.DeploymentSpec{},
					Status: appsv1.DeploymentStatus{},
				}, &pdb, &clusterConfigFullHA),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						HighAvailabilityMode: configv1.FullHighAvailabilityMode},
				}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 0, wantErr: false, expectedReplicaCount: 3,
		},

		{
			name: "test ensureEtcdGuardDeployment - deployment not exists but pdb exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(&clusterConfigFullHA, &pdb),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						HighAvailabilityMode: configv1.FullHighAvailabilityMode},
				}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 2, wantErr: false, expectedReplicaCount: 3,
		},

		{
			name: "test ensureEtcdGuardDeployment - nonHAmod",
			fields: fields{
				client: fakecore.NewSimpleClientset(),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{
						HighAvailabilityMode: configv1.NoneHighAvailabilityMode},
				}).ConfigV1()},
			expectedHAmode: configv1.NoneHighAvailabilityMode, expectedEvents: 0, wantErr: false, expectedReplicaCount: 0,
		},

		{
			name: "test ensureEtcdGuardDeployment - ha mod not set, nothing exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(&clusterConfigFullHA),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{}}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 4, wantErr: false, expectedReplicaCount: 3,
		},

		{
			name: "test ensureEtcdGuardDeployment - 5 replicas and nothing exists",
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
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{}}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 4, wantErr: false, expectedReplicaCount: 5,
		},

		{
			name: "test ensureEtcdGuardDeployment - get clusterConfig not exists",
			fields: fields{
				client: fakecore.NewSimpleClientset(),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{}}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 0, wantErr: true,
		},

		{
			name: "test ensureEtcdGuardDeployment - get replicas count key not found",
			fields: fields{
				client: fakecore.NewSimpleClientset(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name:      clusterConfigName,
						Namespace: clusterConfigNamespace,
					}, Data: map[string]string{clusterConfigKey: `apiVersion: v1
controlPlane:
  hyperthreading: Enabled
  name: master`}}),
				clientInf: fakeconfig.NewSimpleClientset(&configv1.Infrastructure{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Name: infrastructureClusterName,
					},
					Status: configv1.InfrastructureStatus{}}).ConfigV1()},
			expectedHAmode: configv1.FullHighAvailabilityMode, expectedEvents: 0, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := events.NewInMemoryRecorder("test")
			c := &QuorumGuardController{
				kubeClient:  tt.fields.client,
				infraClient: tt.fields.clientInf,
			}
			err := c.ensureEtcdGuardDeployment(context.TODO(), recorder)
			if (err != nil) != tt.wantErr {
				t.Errorf("ensureEtcdGuardDeployment() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if len(recorder.Events()) != tt.expectedEvents {
				t.Errorf("number of events %d and expected %d", len(recorder.Events()), tt.expectedEvents)
				return
			}

			if c.haMode != tt.expectedHAmode {
				t.Errorf("HA mode is %s and expected %s", c.haMode, tt.expectedHAmode)
				return
			}

			if c.replicaCount != tt.expectedReplicaCount {
				t.Errorf("replicaCount is %d and expected is %d", c.replicaCount, tt.expectedReplicaCount)
				return
			}
		})
	}
}

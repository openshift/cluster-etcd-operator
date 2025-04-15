package config

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

type args struct {
	ctx               context.Context
	kubeClient        kubernetes.Interface
	etcdImagePullSpec string
}

func TestGetClusterConfig(t *testing.T) {
	tests := []struct {
		name    string
		args    args
		want    ClusterConfig
		wantErr bool
	}{
		{
			name: "default",
			args: getArgs(t, []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							// ensure that internal IP is used
							{Type: corev1.NodeExternalIP, Address: "xxx"},
							{Type: corev1.NodeInternalIP, Address: "IP1"},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test2",
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							// fallback to 1st IP incase no internal IP is set
							{Type: corev1.NodeExternalIP, Address: "IP2"},
						},
					},
				},
			}, "myEtcdImage"),
			want: ClusterConfig{
				NodeName1:    "test1",
				NodeName2:    "test2",
				NodeIP1:      "IP1",
				NodeIP2:      "IP2",
				EtcdPullSpec: "myEtcdImage",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetClusterConfig(tt.args.ctx, tt.args.kubeClient, tt.args.etcdImagePullSpec)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetClusterConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetClusterConfig() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func getArgs(t *testing.T, nodes []*corev1.Node, etcdPullImage string) args {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fakeKubeClient := fake.NewSimpleClientset()
	for _, node := range nodes {
		_, err := fakeKubeClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
		if err != nil {
			t.Errorf("error creating fake node: %v", err)
		}
	}

	return args{
		ctx:               ctx,
		kubeClient:        fakeKubeClient,
		etcdImagePullSpec: etcdPullImage,
	}
}

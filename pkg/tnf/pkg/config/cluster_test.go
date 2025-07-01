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
	ctx        context.Context
	kubeClient kubernetes.Interface
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
						Labels: map[string]string{
							"node-role.kubernetes.io/master": "",
						},
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
						Labels: map[string]string{
							"node-role.kubernetes.io/master": "",
						},
					},
					Status: corev1.NodeStatus{
						Addresses: []corev1.NodeAddress{
							// fallback to 1st IP incase no internal IP is set
							{Type: corev1.NodeExternalIP, Address: "IP2"},
						},
					},
				},
			}),
			want: ClusterConfig{
				NodeName1: "test1",
				NodeName2: "test2",
				NodeIP1:   "IP1",
				NodeIP2:   "IP2",
			},
			wantErr: false,
		},
		{
			name: "one node only should fail",
			args: getArgs(t, []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
						Labels: map[string]string{
							"node-role.kubernetes.io/master": "",
						},
					},
				},
			}),
			want:    ClusterConfig{},
			wantErr: true,
		},
		{
			name: "one control plane node only should fail",
			args: getArgs(t, []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test1",
						Labels: map[string]string{
							"node-role.kubernetes.io/master": "",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test2",
						Labels: map[string]string{
							"node-role.kubernetes.io/no-master": "",
						},
					},
				},
			}),
			want:    ClusterConfig{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetClusterConfig(tt.args.ctx, tt.args.kubeClient)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetClusterConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetClusterConfig() got = %v, want %v", got, tt.want)
			}
			// delete nodes
			c := tt.args.kubeClient
			nodes, _ := c.CoreV1().Nodes().List(tt.args.ctx, metav1.ListOptions{})
			for _, node := range nodes.Items {
				c.CoreV1().Nodes().Delete(tt.args.ctx, node.Name, metav1.DeleteOptions{})
			}
		})
	}
}

func getArgs(t *testing.T, nodes []*corev1.Node) args {
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
		ctx:        ctx,
		kubeClient: fakeKubeClient,
	}
}

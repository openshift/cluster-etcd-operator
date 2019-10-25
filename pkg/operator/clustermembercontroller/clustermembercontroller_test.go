package clustermembercontroller

import (
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"
)

func TestClusterMemberController_RemoveBootstrapFromEndpoint(t *testing.T) {

	client := fake.NewSimpleClientset()

	addressList := []v1.EndpointAddress{
		{
			IP:       "192.168.2.1",
			Hostname: "etcd-bootstrap",
		},
		{
			IP:       "192.168.2.2",
			Hostname: "etcd-1",
		},
		{
			IP:       "192.168.2.3",
			Hostname: "etcd-2",
		},
		{
			IP:       "192.168.2.4",
			Hostname: "etcd-3",
		},
	}
	ep := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      EtcdHostEndpointName,
			Namespace: EtcdEndpointNamespace,
		},
		Subsets: []v1.EndpointSubset{
			{
				Addresses: addressList,
			},
		},
	}

	_, err := client.CoreV1().Endpoints(EtcdEndpointNamespace).Create(ep)
	if err != nil {
		t.Fatal()
	}

	type fields struct {
		clientset            kubernetes.Interface
		operatorConfigClient v1helpers.OperatorClient
		queue                workqueue.RateLimitingInterface
		eventRecorder        events.Recorder
		etcdDiscoveryDomain  string
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{
			name: "remove 0th address",
			fields: fields{
				clientset:           client,
				etcdDiscoveryDomain: "",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ClusterMemberController{
				clientset:            tt.fields.clientset,
				operatorConfigClient: tt.fields.operatorConfigClient,
				queue:                tt.fields.queue,
				eventRecorder:        tt.fields.eventRecorder,
				etcdDiscoveryDomain:  tt.fields.etcdDiscoveryDomain,
			}
			if err := c.RemoveBootstrapFromEndpoint(); (err != nil) != tt.wantErr {
				t.Errorf("RemoveBootstrapFromEndpoint() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

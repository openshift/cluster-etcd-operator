package clustermembercontroller

import (
	"bytes"
	"encoding/json"

	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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

func getBytes(obj interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func Test_isClusterEtcdOperatorReady(t *testing.T) {
	//Todo: refactor this by making a function for getting observedConfig
	node := "ip-10-0-139-142.ec2.internal"
	clusterDomain := "operator.testing.openshift"
	clusterMemberPath := []string{"cluster", "pending"}
	var etcdURLs []interface{}
	observedConfig := map[string]interface{}{}
	etcdURL := map[string]interface{}{}

	if err := unstructured.SetNestedField(etcdURL, node, "name"); err != nil {
		t.Fatalf("error occured in writing nested fields %#v", err)
	}
	if err := unstructured.SetNestedField(etcdURL, "https://etcd-1."+clusterDomain+":2380", "peerURLs"); err != nil {
		t.Fatalf("error occured in writing nested fields %#v", err)
	}
	if err := unstructured.SetNestedField(etcdURL, string(ceoapi.MemberUnknown), "status"); err != nil {
		t.Fatalf("error occured in writing nested fields %#v", err)
	}
	etcdURLs = append(etcdURLs, etcdURL)
	if err := unstructured.SetNestedField(observedConfig, etcdURLs, clusterMemberPath...); err != nil {
		t.Fatalf("error occured in writing nested fields observedConfig: %#v", err)
	}
	etcdURLsBytes, err := getBytes(observedConfig)
	if err != nil {
		t.Fatalf("error occured in getting bytes for etcdURLs: %#v", err)
	}

	emptyObservedConfig := map[string]interface{}{}
	emptyEtcdURLs := []interface{}{}

	if err := unstructured.SetNestedField(emptyObservedConfig, emptyEtcdURLs, clusterMemberPath...); err != nil {
		t.Fatalf("error occured in writing nested fields observedConfig: %#v", err)
	}
	emptyEtcdURLsBytes, err := getBytes(emptyObservedConfig)
	type args struct {
		originalSpec *operatorv1.OperatorSpec
	}

	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "test with 1 pending member",
			args: args{
				originalSpec: &operatorv1.OperatorSpec{
					ObservedConfig: runtime.RawExtension{
						Raw: etcdURLsBytes,
					},
				},
			},
			want: false,
		},
		{
			name: "test with 0 pending member",
			args: args{
				originalSpec: &operatorv1.OperatorSpec{
					ObservedConfig: runtime.RawExtension{
						Raw: emptyEtcdURLsBytes,
					},
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClusterEtcdOperatorReady(tt.args.originalSpec); got != tt.want {
				t.Errorf("isClusterEtcdOperatorReady() = %v, want %v", got, tt.want)
			}
		})
	}
}

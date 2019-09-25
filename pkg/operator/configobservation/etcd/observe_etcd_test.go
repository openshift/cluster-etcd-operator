package etcd

import (
	"reflect"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func fakeObjectReference(ep *v1.Endpoints) *v1.ObjectReference {
	return &v1.ObjectReference{
		Kind:            ep.Kind,
		Namespace:       ep.Namespace,
		Name:            ep.Name,
		UID:             ep.UID,
		APIVersion:      ep.APIVersion,
		ResourceVersion: ep.ResourceVersion,
	}
}

func TestObservePendingClusterMembers(t *testing.T) {
	node := "ip-10-0-139-142.ec2.internal"
	podIP := "10.0.139.142"
	clusterMemberPath := []string{"cluster", "pending"}
	var etcdURLs []interface{}
	observedConfig := map[string]interface{}{}
	etcdURL := map[string]interface{}{}
	if err := unstructured.SetNestedField(etcdURL, node, "name"); err != nil {
		t.Fatalf("error occured in writing nested fields %#v", err)
	}
	if err := unstructured.SetNestedField(etcdURL, "https://"+podIP+":2380", "peerURLs"); err != nil {
		t.Fatalf("error occured in writing nested fields %#v", err)
	}

	client := fake.NewSimpleClientset()

	etcdURLs = append(etcdURLs, etcdURL)
	if err := unstructured.SetNestedField(observedConfig, etcdURLs, clusterMemberPath...); err != nil {
		t.Fatalf("error occured in writing nested fields observedConfig: %#v", err)
	}
	addressList := []v1.EndpointAddress{
		{
			IP:       podIP,
			Hostname: "",
			NodeName: &node,
			TargetRef: &v1.ObjectReference{
				Name: node,
			},
		},
	}
	ep := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd",
			Namespace: "openshift-etcd",
		},
		Subsets: []v1.EndpointSubset{
			{
				NotReadyAddresses: addressList,
			},
		},
	}

	endpointCache := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	epLister := corev1listers.NewEndpointsLister(endpointCache)
	err := endpointCache.Add(ep)
	if err != nil {
		t.Fatal()
	}
	_, err = epLister.Endpoints("openshift-etcd").Get("etcd")
	if err != nil {
		t.Fatalf("error getting endpoint %#v", err)
	}
	c := configobservation.Listers{
		OpenshiftEtcdEndpointsLister: epLister,
	}

	r := events.NewRecorder(client.CoreV1().Events("test-namespace"), "test-operator",
		fakeObjectReference(ep))

	type args struct {
		genericListers configobserver.Listers
		recorder       events.Recorder
		currentConfig  map[string]interface{}
	}
	tests := []struct {
		name               string
		args               args
		wantObservedConfig map[string]interface{}
		wantErrs           []error
		runAfter           func()
	}{
		// TODO: Refine the test cases.
		{
			name: "bootstrapping test case",
			args: args{
				genericListers: c,
				recorder:       r,
				currentConfig:  make(map[string]interface{}),
			},
			wantObservedConfig: observedConfig,
			wantErrs:           nil,
			runAfter: func() {
				ep.Subsets[0].NotReadyAddresses = []v1.EndpointAddress{}
				ep.Subsets[0].Addresses = addressList
				err := endpointCache.Update(ep)
				if err != nil {
					t.Fatalf("error updating endpoint %v", err)
				}
			},
		},
		{
			name: "Bootstrapping complete test case",
			args: args{
				genericListers: c,
				recorder:       r,
				currentConfig:  observedConfig,
			},
			wantObservedConfig: map[string]interface{}{},
			wantErrs:           nil,
			runAfter: func() {

			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotObservedConfig, gotErrs := ObservePendingClusterMembers(tt.args.genericListers, tt.args.recorder, tt.args.currentConfig)
			if !reflect.DeepEqual(gotObservedConfig, tt.wantObservedConfig) {
				t.Errorf("ObservePendingClusterMembers() gotObservedConfig = %s, want %s", gotObservedConfig, tt.wantObservedConfig)
			}
			if !reflect.DeepEqual(gotErrs, tt.wantErrs) {
				t.Errorf("ObservePendingClusterMembers() gotErrs = %v, want %v", gotErrs, tt.wantErrs)
			}
			tt.runAfter()
		})
	}
}

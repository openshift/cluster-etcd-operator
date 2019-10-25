package etcd

import (
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"

	"reflect"
	"testing"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
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

	etcdEndpoint := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd",
			Namespace: clustermembercontroller.EtcdEndpointNamespace,
		},
		Subsets: []v1.EndpointSubset{
			{
				NotReadyAddresses: addressList,
			},
		},
	}

	etcdHostEndpoint := &v1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "host-etcd",
			Namespace:   clustermembercontroller.EtcdEndpointNamespace,
			Annotations: map[string]string{"alpha.installer.openshift.io/dns-suffix": clusterDomain},
		},
		Subsets: []v1.EndpointSubset{
			{
				NotReadyAddresses: addressList,
			},
		},
	}

	memberConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   clustermembercontroller.EtcdEndpointNamespace,
			Name:        "member-config",
			Annotations: map[string]string{clustermembercontroller.EtcdScalingAnnotationKey: ""},
		},
	}

	etcdMemberPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ip-10-0-139-142.ec2.internal",
			Namespace: clustermembercontroller.EtcdEndpointNamespace,
		},
	}

	index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if err := index.Add(etcdEndpoint); err != nil {
		t.Fatal()
	}
	if err := index.Add(etcdHostEndpoint); err != nil {
		t.Fatal()
	}
	if err := index.Add(memberConfigMap); err != nil {
		t.Fatal()
	}
	if err := index.Add(etcdMemberPod); err != nil {
		t.Fatal()
	}
	c := configobservation.Listers{
		OpenshiftEtcdEndpointsLister:  corev1listers.NewEndpointsLister(index),
		OpenshiftEtcdConfigMapsLister: corev1listers.NewConfigMapLister(index),
		OpenshiftEtcdPodsLister:       corev1listers.NewPodLister(index),
	}
	client := fake.NewSimpleClientset()
	r := events.NewRecorder(client.CoreV1().Events("test-namespace"), "test-operator",
		fakeObjectReference(etcdEndpoint))

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
				etcdEndpoint.Subsets[0].NotReadyAddresses = []v1.EndpointAddress{}
				etcdEndpoint.Subsets[0].Addresses = addressList
				err := index.Update(etcdEndpoint)
				if err != nil {
					t.Fatalf("error updating endpoint %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotObservedConfig, gotErrs := ObservePendingClusterMembers(tt.args.genericListers, tt.args.recorder, tt.args.currentConfig)
			if !reflect.DeepEqual(gotErrs, tt.wantErrs) {
				t.Errorf("ObservePendingClusterMembers() gotErrs = %v, want %v", gotErrs, tt.wantErrs)
			}
			if !reflect.DeepEqual(gotObservedConfig, tt.wantObservedConfig) {
				t.Errorf("ObservePendingClusterMembers() gotObservedConfig = %s, want %s", gotObservedConfig, tt.wantObservedConfig)
			}
			tt.runAfter()
		})
	}
}

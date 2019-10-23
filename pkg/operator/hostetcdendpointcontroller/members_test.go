package hostetcdendpointcontroller

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	v1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func Test_getHostname(t *testing.T) {
	type args struct {
		peerURLs []string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "valid test case for etcd member",
			args: args{peerURLs: []string{"https://etcd-0.foouser.tests.com"}},
			want: "etcd-0",
		},
		{
			name: "valid test case for etcd bootstrap node",
			args: args{peerURLs: []string{"https://etcd-bootstrap.foouser.tests.com"}},
			want: "etcd-bootstrap",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getEtcdName(tt.args.peerURLs); got != tt.want {
				t.Errorf("getHostname() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TODO: alaypatel07 refine the commented test with fake a etcd server.
//func Test_healthyEtcdMemberGetter_EtcdList(t *testing.T) {
//	node := "ip-10-0-139-142.ec2.internal"
//	peerURL := "https://etcd-0.foouser.test.com:2380"
//	//podIP := "10.0.139.142"
//	clusterMemberPath := []string{"cluster", "members"}
//
//	var etcdURLs []interface{}
//	observedConfig := map[string]interface{}{}
//	etcdURL := map[string]interface{}{}
//	if err := unstructured.SetNestedField(etcdURL, node, "name"); err != nil {
//		t.Fatalf("error occured in writing nested fields %#v", err)
//	}
//	if err := unstructured.SetNestedField(etcdURL, peerURL, "peerURLs"); err != nil {
//		t.Fatalf("error occured in writing nested fields %#v", err)
//	}
//
//	if err := unstructured.SetNestedField(etcdURL, string(ceoapi.MemberUnknown), "status"); err != nil {
//		t.Fatalf("error occured in writing nested fields observedConfig: %#v", err)
//	}
//
//	etcdURLs = append(etcdURLs, etcdURL)
//	if err := unstructured.SetNestedField(observedConfig, etcdURLs, clusterMemberPath...); err != nil {
//		t.Fatalf("error occured in writing nested fields observedConfig: %#v", err)
//	}
//
//	b := &bytes.Buffer{}
//	e := json.NewEncoder(b)
//	err := e.Encode(observedConfig)
//
//	if err != nil {
//		t.Fatalf("err encoding observedConfig %#v", err)
//	}
//
//	etcdSpec := v1.StaticPodOperatorSpec{
//		OperatorSpec: v1.OperatorSpec{
//			ObservedConfig: runtime.RawExtension{
//				Raw: b.Bytes(),
//			},
//		},
//	}
//
//	fakeOperatorClient := v1helpers.NewFakeOperatorClient(&etcdSpec.OperatorSpec, nil, nil)
//
//	type fields struct {
//		operatorConfigClient v1helpers.OperatorClient
//	}
//	type args struct {
//		bucket string
//	}
//	tests := []struct {
//		name    string
//		fields  fields
//		args    args
//		want    []ceoapi.Member
//		wantErr bool
//	}{
//		{
//			name:   "valid test case",
//			fields: fields{operatorConfigClient: fakeOperatorClient},
//			args:   args{bucket: "members"},
//			want: []ceoapi.Member{
//				{
//					Name:     node,
//					PeerURLS: []string{peerURL},
//					Conditions: []ceoapi.MemberCondition{
//						{
//							// We can force a ReadyCondition here but because of bootstrap exception
//							// need to relax checking of condition and assume that
//							// all members in the cluster.members will be healthy.
//							Type: ceoapi.MemberUnknown,
//						},
//					},
//				},
//			},
//		},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			h := &healthyEtcdMemberGetter{
//				operatorConfigClient: tt.fields.operatorConfigClient,
//			}
//			got, err := h.EtcdList(tt.args.bucket)
//			if (err != nil) != tt.wantErr {
//				t.Errorf("EtcdList() error = %v, wantErr %v", err, tt.wantErr)
//				return
//			}
//			if !reflect.DeepEqual(got, tt.want) {
//				t.Errorf("EtcdList() got = %v, want %v", got, tt.want)
//			}
//		})
//	}
//}

func Test_healthyEtcdMemberGetter_Endpoints(t *testing.T) {
	storageURLs := []string{
		"https://etcd-0.foouser.test.com:2380",
		"https://etcd-1.foouser.test.com:2380",
		"https://etcd-2.foouser.test.com:2380",
	}

	//podIP := "10.0.139.142"
	storageConfigURLsPath := []string{"storageConfig", "urls"}

	observedConfig := map[string]interface{}{}
	if err := unstructured.SetNestedStringSlice(observedConfig, storageURLs, storageConfigURLsPath...); err != nil {
		t.Fatalf("error occured in writing nested fields observedConfig: %#v", err)
	}

	b := &bytes.Buffer{}
	e := json.NewEncoder(b)
	err := e.Encode(observedConfig)

	if err != nil {
		t.Fatalf("err encoding observedConfig %#v", err)
	}

	etcdSpec := v1.StaticPodOperatorSpec{
		OperatorSpec: v1.OperatorSpec{
			ObservedConfig: runtime.RawExtension{
				Raw: b.Bytes(),
			},
		},
	}

	fakeOperatorClient := v1helpers.NewFakeOperatorClient(&etcdSpec.OperatorSpec, nil, nil)

	type fields struct {
		operatorConfigClient v1helpers.OperatorClient
	}
	tests := []struct {
		name    string
		fields  fields
		want    []string
		wantErr bool
	}{
		{
			name:    "valid test case",
			fields:  fields{operatorConfigClient: fakeOperatorClient},
			want:    storageURLs,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &healthyEtcdMemberGetter{
				operatorConfigClient: tt.fields.operatorConfigClient,
			}
			got, err := h.Endpoints()
			if (err != nil) != tt.wantErr {
				t.Errorf("Endpoints() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Endpoints() got = %v, want %v", got, tt.want)
			}
		})
	}
}

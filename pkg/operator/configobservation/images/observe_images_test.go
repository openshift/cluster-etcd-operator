package images

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	configv1 "github.com/openshift/api/config/v1"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
)

type imageConfigTest struct {
	imageConfig                       *configv1.Image
	expectedInternalRegistryHostname  string
	expectedExternalRegistryHostnames []string
	expectedAllowedRegistries         []configv1.RegistryLocation
	expectedEventReasons              []string
}

func TestObserveImageConfig(t *testing.T) {
	allowedRegistries := []configv1.RegistryLocation{
		{
			DomainName: "insecuredomain",
			Insecure:   true,
		},
		{
			DomainName: "securedomain1",
			Insecure:   true,
		},
		{
			DomainName: "securedomain2",
		},
	}

	tests := []imageConfigTest{
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.ImageStatus{
					InternalRegistryHostname: "docker-registry.openshift-image-registry.svc.cluster.local:5000",
				},
			},
			expectedInternalRegistryHostname: "docker-registry.openshift-image-registry.svc.cluster.local:5000",
			expectedEventReasons:             []string{"ObserveInternalRegistryHostnameChanged"},
		},
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: configv1.ImageSpec{
					ExternalRegistryHostnames: []string{},
				},
			},
			expectedExternalRegistryHostnames: nil,
			expectedEventReasons:              []string{},
		},
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: configv1.ImageSpec{
					ExternalRegistryHostnames: []string{"spec.external.host.com"},
				},
			},
			expectedExternalRegistryHostnames: []string{"spec.external.host.com"},
			expectedEventReasons:              []string{"ObserveExternalRegistryHostnameChanged"},
		},
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Status: configv1.ImageStatus{
					ExternalRegistryHostnames: []string{"status.external.host.com"},
				},
			},
			expectedExternalRegistryHostnames: []string{"status.external.host.com"},
			expectedEventReasons:              []string{"ObserveExternalRegistryHostnameChanged"},
		},
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: configv1.ImageSpec{
					ExternalRegistryHostnames: []string{"spec.external.host.com"},
				},
				Status: configv1.ImageStatus{
					ExternalRegistryHostnames: []string{"status.external.host.com"},
				},
			},
			expectedExternalRegistryHostnames: []string{"spec.external.host.com", "status.external.host.com"},
			expectedEventReasons:              []string{"ObserveExternalRegistryHostnameChanged"},
		},
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: configv1.ImageSpec{
					AllowedRegistriesForImport: allowedRegistries,
				},
			},
			expectedAllowedRegistries: allowedRegistries,
			expectedEventReasons:      []string{"ObserveAllowedRegistriesForImport"},
		},
		{
			imageConfig: &configv1.Image{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster",
				},
				Spec: configv1.ImageSpec{
					AllowedRegistriesForImport: []configv1.RegistryLocation{},
				},
			},
			expectedAllowedRegistries: nil,
			expectedEventReasons:      []string{},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			indexer.Add(tc.imageConfig)
			listers := configobservation.Listers{
				ImageConfigLister: configlistersv1.NewImageLister(indexer),
			}
			eventRecorder := events.NewInMemoryRecorder("")

			initialExistingConfig := map[string]interface{}{}

			observed, errs := ObserveInternalRegistryHostname(listers, eventRecorder, initialExistingConfig)
			if len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
			internalRegistryHostname, _, err := unstructured.NestedString(observed, "imagePolicyConfig", "internalRegistryHostname")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if internalRegistryHostname != tc.expectedInternalRegistryHostname {
				t.Errorf("expected internal registry hostname: %s, got %s", tc.expectedInternalRegistryHostname, internalRegistryHostname)
			}
			secondTimeObserved, errs := ObserveInternalRegistryHostname(listers, eventRecorder, observed)
			if len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
			if !reflect.DeepEqual(observed, secondTimeObserved) {
				t.Errorf("unexpected change after second observation: got: \n%#v\nexpected: \n%#v", secondTimeObserved, observed)
			}

			observed, errs = ObserveExternalRegistryHostnames(listers, eventRecorder, initialExistingConfig)
			if len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
			o, _, err := unstructured.NestedSlice(observed, "imagePolicyConfig", "externalRegistryHostnames")
			buf := &bytes.Buffer{}
			if err := json.NewEncoder(buf).Encode(o); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			externalRegistryHostnames := []string{}
			if err := json.NewDecoder(buf).Decode(&externalRegistryHostnames); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(externalRegistryHostnames, tc.expectedExternalRegistryHostnames) {
				t.Errorf("got: \n%#v\nexpected: \n%#v", externalRegistryHostnames, tc.expectedExternalRegistryHostnames)
			}
			secondTimeObserved, errs = ObserveExternalRegistryHostnames(listers, eventRecorder, observed)
			if len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
			if !reflect.DeepEqual(observed, secondTimeObserved) {
				t.Errorf("unexpected change after second observation: got: \n%#v\nexpected: \n%#v", secondTimeObserved, observed)
			}

			observed, errs = ObserveAllowedRegistriesForImport(listers, eventRecorder, initialExistingConfig)
			if len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
			o, _, err = unstructured.NestedSlice(observed, "imagePolicyConfig", "allowedRegistriesForImport")
			buf = &bytes.Buffer{}
			if err := json.NewEncoder(buf).Encode(o); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			allowedRegistries := []configv1.RegistryLocation{}
			if err := json.NewDecoder(buf).Decode(&allowedRegistries); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(allowedRegistries, tc.expectedAllowedRegistries) {
				t.Errorf("got: \n%#v\nexpected: \n%#v", allowedRegistries, tc.expectedAllowedRegistries)
			}
			secondTimeObserved, errs = ObserveAllowedRegistriesForImport(listers, eventRecorder, observed)
			if len(errs) != 0 {
				t.Fatalf("unexpected error: %v", errs)
			}
			if !reflect.DeepEqual(observed, secondTimeObserved) {
				t.Errorf("unexpected change after second observation: got: \n%#v\nexpected: \n%#v", secondTimeObserved, observed)
			}

			// Check created events
			reasons := []string{}
			for _, ev := range eventRecorder.Events() {
				reasons = append(reasons, ev.Reason)
			}
			if got, expected := reasons, tc.expectedEventReasons; !equality.Semantic.DeepEqual(got, expected) {
				t.Errorf("unexpected events, got reasons: %v, expected: %v", got, expected)
			}
		})
	}
}

package apiserver

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/client-go/tools/cache"

	configv1 "github.com/openshift/api/config/v1"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resourcesynccontroller"

	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/configobservation"
	"github.com/openshift/cluster-kube-apiserver-operator/pkg/operator/operatorclient"
)

func TestObserveUserClientCABundle(t *testing.T) {

	testCases := []struct {
		name           string
		config         *configv1.APIServer
		existing       map[string]interface{}
		expected       map[string]interface{}
		expectedSynced map[string]string
	}{
		{
			name:     "NoAPIServerConfig",
			config:   nil,
			existing: map[string]interface{}{},
			expected: map[string]interface{}{},
			expectedSynced: map[string]string{
				"configmap/user-client-ca.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name:     "NoUserClientCARef",
			config:   newAPIServerConfig(),
			existing: map[string]interface{}{},
			expected: map[string]interface{}{},
			expectedSynced: map[string]string{
				"configmap/user-client-ca.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name:     "HappyPath",
			config:   newAPIServerConfig(withClientCA("happy")),
			existing: map[string]interface{}{},
			expected: map[string]interface{}{},
			expectedSynced: map[string]string{
				"configmap/user-client-ca.openshift-kube-apiserver": "configmap/happy.openshift-config",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if tc.config != nil {
				if err := indexer.Add(tc.config); err != nil {
					t.Fatal(err)
				}
			}
			synced := map[string]string{}
			listers := configobservation.Listers{
				APIServerLister: configlistersv1.NewAPIServerLister(indexer),
				ResourceSync:    &mockResourceSyncer{t: t, synced: synced},
			}
			result, errs := ObserveUserClientCABundle(listers, events.NewInMemoryRecorder(t.Name()), tc.existing)
			if len(errs) > 0 {
				t.Errorf("Expected 0 errors, got %v.", len(errs))
			}
			if !equality.Semantic.DeepEqual(tc.expected, result) {
				t.Errorf("did not expect observed config to be updated : %s", result)
			}
			if !equality.Semantic.DeepEqual(tc.expectedSynced, synced) {
				t.Errorf("expected resources not synced: %s", diff.ObjectReflectDiff(tc.expectedSynced, synced))
			}
		})
	}
}

func TestObserveDefaultServingCertificate(t *testing.T) {

	existingConfig := map[string]interface{}{
		"servingInfo": map[string]interface{}{
			"certFile": "/etc/kubernetes/static-pod-certs/secrets/existing/tls.key",
		},
	}

	testCases := []struct {
		name           string
		config         *configv1.APIServer
		existing       map[string]interface{}
		expected       map[string]interface{}
		expectedSynced map[string]string
	}{
		{
			name:           "NoAPIServerConfig",
			config:         nil,
			existing:       existingConfig,
			expected:       map[string]interface{}{},
			expectedSynced: map[string]string{"configmap/user-serving-cert.openshift-kube-apiserver": "DELETE"},
		},
		{
			name:           "NoUserServingCertRef",
			config:         newAPIServerConfig(),
			existing:       existingConfig,
			expected:       map[string]interface{}{},
			expectedSynced: map[string]string{"configmap/user-serving-cert.openshift-kube-apiserver": "DELETE"},
		},
		{
			name:     "HappyPath",
			config:   newAPIServerConfig(withDefaultSecret("happy")),
			existing: existingConfig,
			expected: map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert/tls.crt",
					"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert/tls.key",
				},
			},
			expectedSynced: map[string]string{
				"configmap/user-serving-cert.openshift-kube-apiserver": "configmap/happy.openshift-config",
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if tc.config != nil {
				if err := indexer.Add(tc.config); err != nil {
					t.Fatal(err)
				}
			}
			synced := map[string]string{}
			listers := configobservation.Listers{
				APIServerLister: configlistersv1.NewAPIServerLister(indexer),
				ResourceSync:    &mockResourceSyncer{t: t, synced: synced},
			}
			result, errs := ObserveDefaultUserServingCertificate(listers, events.NewInMemoryRecorder(t.Name()), tc.existing)
			if len(errs) > 0 {
				t.Errorf("Expected 0 errors, got %v.", len(errs))
			}
			if !equality.Semantic.DeepEqual(tc.expected, result) {
				t.Errorf("result does not match expected config: %s", diff.ObjectDiff(tc.expected, result))
			}
			if !equality.Semantic.DeepEqual(tc.expectedSynced, synced) {
				t.Errorf("expected resources not synced: %s", diff.ObjectReflectDiff(tc.expectedSynced, synced))
			}
		})
	}
}

func TestObserveNamedCertificates(t *testing.T) {

	existingConfig := map[string]interface{}{
		"servingInfo": map[string]interface{}{
			"namedCertificates": []interface{}{
				map[string]interface{}{
					"certFile": "/etc/kubernetes/static-pod-certs/secrets/existing/tls.crt",
					"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/existing/tls.key",
					"names":    []interface{}{"existing"},
				},
			},
		},
	}

	testCases := []struct {
		name           string
		config         *configv1.APIServer
		existing       map[string]interface{}
		expected       map[string]interface{}
		expectErrs     bool
		expectedSynced map[string]string
	}{
		{
			name:     "NoAPIServerConfig",
			config:   nil,
			existing: existingConfig,
			expected: map[string]interface{}{},
			expectedSynced: map[string]string{
				"secret/user-serving-cert-000.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-001.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-002.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-003.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-004.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-005.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-006.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-007.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-008.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-009.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name:     "NoNamedCertificates",
			config:   newAPIServerConfig(),
			existing: existingConfig,
			expected: map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"namedCertificates": []interface{}{
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.key",
						},
					},
				},
			},
			expectedSynced: map[string]string{
				"secret/user-serving-cert-000.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-001.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-002.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-003.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-004.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-005.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-006.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-007.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-008.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-009.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name: "NamedCertificateWithName",
			config: newAPIServerConfig(
				withCertificate(
					withName("*.foo.org"),
					withSecret("foo"),
				),
			),
			existing: existingConfig,
			expected: map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"namedCertificates": []interface{}{
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.key",
							"names":    []interface{}{"*.foo.org"},
						},
					},
				},
			},
			expectedSynced: map[string]string{
				"secret/user-serving-cert-000.openshift-kube-apiserver": "secret/foo.openshift-config",
				"secret/user-serving-cert-001.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-002.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-003.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-004.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-005.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-006.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-007.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-008.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-009.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name: "NamedCertificateWithoutName",
			config: newAPIServerConfig(
				withCertificate(
					withSecret("foo"),
				),
			),
			existing: existingConfig,
			expected: map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"namedCertificates": []interface{}{
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.key",
						},
					},
				},
			},
			expectedSynced: map[string]string{
				"secret/user-serving-cert-000.openshift-kube-apiserver": "secret/foo.openshift-config",
				"secret/user-serving-cert-001.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-002.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-003.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-004.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-005.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-006.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-007.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-008.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-009.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name: "NamedCertificateWithNames",
			config: newAPIServerConfig(
				withCertificate(
					withName("*.foo.org"),
					withName("foo.org"),
					withName("*.bar.org"),
					withSecret("foo"),
				),
			),
			existing: existingConfig,
			expected: map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"namedCertificates": []interface{}{
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.key",
							"names":    []interface{}{"*.foo.org", "foo.org", "*.bar.org"},
						},
					},
				},
			},
			expectedSynced: map[string]string{
				"secret/user-serving-cert-000.openshift-kube-apiserver": "secret/foo.openshift-config",
				"secret/user-serving-cert-001.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-002.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-003.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-004.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-005.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-006.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-007.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-008.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-009.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name: "NamedCertificates",
			config: newAPIServerConfig(
				withCertificate(
					withName("one"),
					withSecret("one"),
				),
				withCertificate(
					withSecret("two"),
				),
				withCertificate(
					withName("three"),
					withName("tři"),
					withSecret("three"),
				),
			),
			existing: existingConfig,
			expected: map[string]interface{}{
				"servingInfo": map[string]interface{}{
					"namedCertificates": []interface{}{
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/localhost-serving-cert-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/service-network-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/external-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/internal-loadbalancer-serving-certkey/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-000/tls.key",
							"names":    []interface{}{"one"},
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-001/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-001/tls.key",
						},
						map[string]interface{}{
							"certFile": "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-002/tls.crt",
							"keyFile":  "/etc/kubernetes/static-pod-certs/secrets/user-serving-cert-002/tls.key",
							"names":    []interface{}{"three", "tři"},
						},
					},
				},
			},
			expectedSynced: map[string]string{
				"secret/user-serving-cert-000.openshift-kube-apiserver": "secret/one.openshift-config",
				"secret/user-serving-cert-001.openshift-kube-apiserver": "secret/two.openshift-config",
				"secret/user-serving-cert-002.openshift-kube-apiserver": "secret/three.openshift-config",
				"secret/user-serving-cert-003.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-004.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-005.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-006.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-007.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-008.openshift-kube-apiserver": "DELETE",
				"secret/user-serving-cert-009.openshift-kube-apiserver": "DELETE",
			},
		},
		{
			name: "NamedCertificateNoSecretRef",
			config: newAPIServerConfig(
				withCertificate(
					withName("*.foo.org"),
				),
			),
			existing:   existingConfig,
			expected:   existingConfig,
			expectErrs: true,
		},
		{
			name: "TooManyNamedCertificates",
			config: newAPIServerConfig(
				withCertificate(withName("000"), withSecret("000")),
				withCertificate(withName("001"), withSecret("001")),
				withCertificate(withName("002"), withSecret("002")),
				withCertificate(withName("003"), withSecret("003")),
				withCertificate(withName("004"), withSecret("004")),
				withCertificate(withName("005"), withSecret("005")),
				withCertificate(withName("006"), withSecret("006")),
				withCertificate(withName("007"), withSecret("007")),
				withCertificate(withName("008"), withSecret("008")),
				withCertificate(withName("009"), withSecret("009")),
				withCertificate(withName("010"), withSecret("010")),
			),
			existing:   existingConfig,
			expected:   existingConfig,
			expectErrs: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if tc.config != nil {
				if err := indexer.Add(tc.config); err != nil {
					t.Fatal(err)
				}
			}

			var objs []runtime.Object
			if tc.config != nil {
				for _, nc := range tc.config.Spec.ServingCerts.NamedCertificates {
					objs = append(objs, &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name:      nc.ServingCertificate.Name,
							Namespace: operatorclient.GlobalUserSpecifiedConfigNamespace,
						},
						Data: map[string][]byte{
							"tls.crt": []byte("FOO"),
							"tls.key": []byte("BAR"),
						},
					})
				}
			}

			synced := map[string]string{}
			listers := configobservation.Listers{
				APIServerLister: configlistersv1.NewAPIServerLister(indexer),
				ResourceSync:    &mockResourceSyncer{t: t, synced: synced},
			}
			result, errs := ObserveNamedCertificates(listers, events.NewInMemoryRecorder(t.Name()), tc.existing)
			if tc.expectErrs && len(errs) == 0 {
				t.Error("Expected errors.", errs)
			}
			if !tc.expectErrs && len(errs) > 0 {
				t.Errorf("Expected 0 errors, got %v.", len(errs))
				for _, err := range errs {
					t.Log(err.Error())
				}
			}

			if !equality.Semantic.DeepEqual(tc.expected, result) {
				t.Errorf("result does not match expected config: %s", diff.ObjectDiff(tc.expected, result))
			}
			if !equality.Semantic.DeepEqual(tc.expectedSynced, synced) {
				t.Errorf("expected resources not synced: %s", diff.ObjectReflectDiff(tc.expectedSynced, synced))
			}
		})
	}

}

type mockResourceSyncer struct {
	t      *testing.T
	synced map[string]string
}

func (rs *mockResourceSyncer) SyncConfigMap(destination, source resourcesynccontroller.ResourceLocation) error {
	if (source == resourcesynccontroller.ResourceLocation{}) {
		rs.synced[fmt.Sprintf("configmap/%v.%v", destination.Name, destination.Namespace)] = "DELETE"
	} else {
		rs.synced[fmt.Sprintf("configmap/%v.%v", destination.Name, destination.Namespace)] = fmt.Sprintf("configmap/%v.%v", source.Name, source.Namespace)
	}
	return nil
}

func (rs *mockResourceSyncer) SyncSecret(destination, source resourcesynccontroller.ResourceLocation) error {
	if (source == resourcesynccontroller.ResourceLocation{}) {
		rs.synced[fmt.Sprintf("secret/%v.%v", destination.Name, destination.Namespace)] = "DELETE"
	} else {
		rs.synced[fmt.Sprintf("secret/%v.%v", destination.Name, destination.Namespace)] = fmt.Sprintf("secret/%v.%v", source.Name, source.Namespace)
	}
	return nil
}

func newAPIServerConfig(builders ...func(*configv1.APIServer)) *configv1.APIServer {
	config := &configv1.APIServer{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	for _, builder := range builders {
		builder(config)
	}
	return config
}

func withCertificate(builders ...func(*configv1.APIServerNamedServingCert)) func(*configv1.APIServer) {
	return func(apiserver *configv1.APIServer) {
		certificate := &configv1.APIServerNamedServingCert{}
		for _, builder := range builders {
			builder(certificate)
		}
		apiserver.Spec.ServingCerts.NamedCertificates = append(apiserver.Spec.ServingCerts.NamedCertificates, *certificate)
	}
}

func withName(name string) func(*configv1.APIServerNamedServingCert) {
	return func(cert *configv1.APIServerNamedServingCert) {
		cert.Names = append(cert.Names, name)
	}
}

func withSecret(name string) func(*configv1.APIServerNamedServingCert) {
	return func(cert *configv1.APIServerNamedServingCert) {
		cert.ServingCertificate.Name = name
	}
}

func withDefaultSecret(name string) func(*configv1.APIServer) {
	return func(apiserver *configv1.APIServer) {
		apiserver.Spec.ServingCerts.DefaultServingCertificate.Name = name
	}
}

func withClientCA(name string) func(*configv1.APIServer) {
	return func(apiserver *configv1.APIServer) {
		apiserver.Spec.ClientCA.Name = name
	}
}

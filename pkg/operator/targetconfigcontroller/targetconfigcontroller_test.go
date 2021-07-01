package targetconfigcontroller

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_checkCSRControllerCAConfigMap(t *testing.T) {
	scenarios := []struct {
		name      string
		configMap *v1.ConfigMap
		wantErr   bool
	}{
		{
			name:      "happy path: cluster-kube-controller-manager-operator has generated cert",
			configMap: csrControllerCAConfigMap("cluster-kube-controller-manager-operator"),
		},
		{
			name:      "cert missing managed fields",
			configMap: &v1.ConfigMap{},
			wantErr:   true,
		},
		{
			name:      "unexpected manager",
			configMap: csrControllerCAConfigMap("foobar"),
			wantErr:   true,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			gotErr := checkCSRControllerCAConfigMap(scenario.configMap)
			if gotErr != nil && !scenario.wantErr {
				t.Errorf("unexpected expected error %v", gotErr)
			}
			if gotErr == nil && scenario.wantErr {
				t.Errorf("expected error got nil")
			}
		})
	}
}

func csrControllerCAConfigMap(manager string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "csr-controller-ca",
			Namespace:     "openshift-config-managed",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: manager}},
		},
	}
}

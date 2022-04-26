package ceohelpers

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func TestReadDesiredControlPlaneReplicaCount(t *testing.T) {
	scenarios := []struct {
		name                             string
		operatorSpec                     operatorv1.StaticPodOperatorSpec
		expectedControlPlaneReplicaCount int
		expectedError                    error
	}{
		{
			name: "with replicas set in the observed config only",
			operatorSpec: operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					ObservedConfig: runtime.RawExtension{Raw: []byte(withWellKnownReplicasCountSet)},
				},
			},
			expectedControlPlaneReplicaCount: 3,
		},

		{
			name: "with replicas set in the UnsupportedConfigOverrides",
			operatorSpec: operatorv1.StaticPodOperatorSpec{
				OperatorSpec: operatorv1.OperatorSpec{
					ObservedConfig:             runtime.RawExtension{Raw: []byte(withWellKnownReplicasCountSet)},
					UnsupportedConfigOverrides: runtime.RawExtension{Raw: []byte(withReplicasCountSetInUnsupportedConfig)},
				},
			},
			expectedControlPlaneReplicaCount: 7,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// test data
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(&scenario.operatorSpec, &operatorv1.StaticPodOperatorStatus{}, nil, nil)

			// act
			actualReplicaCount, err := ReadDesiredControlPlaneReplicasCount(fakeOperatorClient)

			// validate
			if err == nil && scenario.expectedError != nil {
				t.Fatal("expected to get an error from readDesiredControlPlaneReplicasCount function")
			}
			if err != nil && scenario.expectedError == nil {
				t.Fatal(err)
			}
			if err != nil && scenario.expectedError != nil && err.Error() != scenario.expectedError.Error() {
				t.Fatalf("unexpected error returned = %v, expected = %v", err, scenario.expectedError)
			}
			if actualReplicaCount != scenario.expectedControlPlaneReplicaCount {
				t.Fatalf("unexpected control plance replicat count: %d, expected: %d", actualReplicaCount, scenario.expectedControlPlaneReplicaCount)
			}
		})
	}
}

var withWellKnownReplicasCountSet = `
{
 "controlPlane": {"replicas": 3}
}
`

var withReplicasCountSetInUnsupportedConfig = `
{
 "controlPlane": {"replicas": 7}
}
`

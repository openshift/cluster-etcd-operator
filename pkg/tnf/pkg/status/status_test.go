package status

import (
	"context"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
)

func TestNewClusterStatus(t *testing.T) {
	ctx := context.Background()
	operatorStatus := &operatorv1.StaticPodOperatorStatus{}
	operatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		operatorStatus,
		nil,
		nil,
	)

	testCases := []struct {
		name                         string
		isExternalEtcd               bool
		bootstrapCompleted           bool
		readyForEtcdTransition       bool
		setBootstrapCompleted        bool
		operatorConditions           []operatorv1.OperatorCondition
		expectedIsExternalEtcd       bool
		expectedIsBootstrap          bool
		expectedIsReadyForTransition bool
	}{
		{
			name:                         "ExternalEtcd disabled",
			isExternalEtcd:               false,
			bootstrapCompleted:           false,
			setBootstrapCompleted:        false,
			readyForEtcdTransition:       false,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd:       false,
			expectedIsBootstrap:          false,
			expectedIsReadyForTransition: false,
		},
		{
			name:                         "ExternalEtcd enabled, bootstrap initially not completed",
			isExternalEtcd:               true,
			bootstrapCompleted:           false,
			setBootstrapCompleted:        false,
			readyForEtcdTransition:       false,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd:       true,
			expectedIsBootstrap:          false,
			expectedIsReadyForTransition: false,
		},
		{
			name:                         "ExternalEtcd enabled, bootstrap initially completed",
			isExternalEtcd:               true,
			bootstrapCompleted:           true,
			setBootstrapCompleted:        false,
			readyForEtcdTransition:       false,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd:       true,
			expectedIsBootstrap:          true,
			expectedIsReadyForTransition: false,
		},
		{
			name:                         "ExternalEtcd enabled, bootstrap set to completed",
			isExternalEtcd:               true,
			bootstrapCompleted:           false,
			setBootstrapCompleted:        true,
			readyForEtcdTransition:       false,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd:       true,
			expectedIsBootstrap:          true,
			expectedIsReadyForTransition: false,
		},
		{
			name:                         "ExternalEtcd enabled, initially ready for etcd transition",
			isExternalEtcd:               true,
			bootstrapCompleted:           true,
			setBootstrapCompleted:        false,
			readyForEtcdTransition:       true,
			operatorConditions:           []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd:       true,
			expectedIsBootstrap:          true,
			expectedIsReadyForTransition: true,
		},
		{
			name:                   "ExternalEtcd enabled, ready for etcd transition by operator condition",
			isExternalEtcd:         true,
			bootstrapCompleted:     true,
			setBootstrapCompleted:  false,
			readyForEtcdTransition: false,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReadyForTransition,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedIsExternalEtcd:       true,
			expectedIsBootstrap:          true,
			expectedIsReadyForTransition: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewClusterStatus(ctx, operatorClient, tc.isExternalEtcd, tc.bootstrapCompleted, tc.readyForEtcdTransition)
			require.NotNil(t, cs, "NewClusterStatus returned nil")

			if tc.setBootstrapCompleted {
				cs.SetBootstrapCompleted()
			}
			operatorStatus.OperatorStatus.Conditions = tc.operatorConditions

			require.Equal(t, tc.expectedIsExternalEtcd, cs.IsExternalEtcdCluster(), "IsExternalEtcdCluster() returned unexpected value")
			require.Equal(t, tc.expectedIsBootstrap, cs.IsBootstrapCompleted(), "IsBootstrapCompleted() returned unexpected value")
			require.Equal(t, tc.expectedIsReadyForTransition, cs.IsReadyForEtcdTransition(), "IsReadyForEtcdTransition() returned unexpected value")
		})
	}
}

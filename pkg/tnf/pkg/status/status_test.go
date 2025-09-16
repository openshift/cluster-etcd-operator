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
		name                   string
		isExternalEtcd         bool
		bootstrapCompleted     bool
		readyForEtcdRemoval    bool
		setBootstrapCompleted  bool
		operatorConditions     []operatorv1.OperatorCondition
		expectedIsExternalEtcd bool
		expectedIsBootstrap    bool
		expectedIsReady        bool
	}{
		{
			name:                   "ExternalEtcd disabled",
			isExternalEtcd:         false,
			bootstrapCompleted:     false,
			setBootstrapCompleted:  false,
			readyForEtcdRemoval:    false,
			operatorConditions:     []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd: false,
			expectedIsBootstrap:    false,
			expectedIsReady:        false,
		},
		{
			name:                   "ExternalEtcd enabled, bootstrap initially not completed",
			isExternalEtcd:         true,
			bootstrapCompleted:     false,
			setBootstrapCompleted:  false,
			readyForEtcdRemoval:    false,
			operatorConditions:     []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd: true,
			expectedIsBootstrap:    false,
			expectedIsReady:        false,
		},
		{
			name:                   "ExternalEtcd enabled, bootstrap initially completed",
			isExternalEtcd:         true,
			bootstrapCompleted:     true,
			setBootstrapCompleted:  false,
			readyForEtcdRemoval:    false,
			operatorConditions:     []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd: true,
			expectedIsBootstrap:    true,
			expectedIsReady:        false,
		},
		{
			name:                   "ExternalEtcd enabled, bootstrap set to completed",
			isExternalEtcd:         true,
			bootstrapCompleted:     false,
			setBootstrapCompleted:  true,
			readyForEtcdRemoval:    false,
			operatorConditions:     []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd: true,
			expectedIsBootstrap:    true,
			expectedIsReady:        false,
		},
		{
			name:                   "ExternalEtcd enabled, initially ready for etcd removal",
			isExternalEtcd:         true,
			bootstrapCompleted:     true,
			setBootstrapCompleted:  false,
			readyForEtcdRemoval:    true,
			operatorConditions:     []operatorv1.OperatorCondition{},
			expectedIsExternalEtcd: true,
			expectedIsBootstrap:    true,
			expectedIsReady:        true,
		},
		{
			name:                  "ExternalEtcd enabled, ready by operator condition",
			isExternalEtcd:        true,
			bootstrapCompleted:    true,
			setBootstrapCompleted: false,
			readyForEtcdRemoval:   false,
			operatorConditions: []operatorv1.OperatorCondition{
				{
					Type:   etcd.OperatorConditionExternalEtcdReady,
					Status: operatorv1.ConditionTrue,
				},
			},
			expectedIsExternalEtcd: true,
			expectedIsBootstrap:    true,
			expectedIsReady:        true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewClusterStatus(ctx, operatorClient, tc.isExternalEtcd, tc.bootstrapCompleted, tc.readyForEtcdRemoval)
			require.NotNil(t, cs, "NewClusterStatus returned nil")

			if tc.setBootstrapCompleted {
				cs.SetBootstrapCompleted()
			}
			operatorStatus.OperatorStatus.Conditions = tc.operatorConditions

			require.Equal(t, tc.expectedIsExternalEtcd, cs.IsExternalEtcdCluster(), "IsExternalEtcdCluster() returned unexpected value")
			require.Equal(t, tc.expectedIsBootstrap, cs.IsBootstrapCompleted(), "IsBootstrapCompleted() returned unexpected value")
			require.Equal(t, tc.expectedIsReady, cs.IsReadyForEtcdRemoval(), "IsReadyForEtcdRemoval() returned unexpected value")
		})
	}
}

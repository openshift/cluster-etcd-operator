package etcd

import (
	"context"
	"testing"

	v1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type args struct {
	ctx            context.Context
	operatorClient v1helpers.StaticPodOperatorClient
}

func TestRemoveStaticContainer(t *testing.T) {
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name:    "sets ReadyForEtcdContainerRemoval condition",
			args:    getArgs(),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := RemoveStaticContainer(tt.args.ctx, tt.args.operatorClient); (err != nil) != tt.wantErr {
				t.Errorf("RemoveStaticContainer() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify that the job condition was set
			_, status, _, err := tt.args.operatorClient.GetStaticPodOperatorState()
			require.NoError(t, err, "Failed to get static pod operator state")
			isSet := v1helpers.IsOperatorConditionTrue(status.Conditions, OperatorConditionExternalEtcdReady)
			require.True(t, isSet, "Expected ReadyForEtcdContainerRemoval condition to be set to True")
		})
	}
}

func getArgs() args {
	ctx := context.Background()

	etcd := &v1.Etcd{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: v1.EtcdSpec{
			StaticPodOperatorSpec: v1.StaticPodOperatorSpec{
				OperatorSpec: v1.OperatorSpec{
					UnsupportedConfigOverrides: runtime.RawExtension{},
				},
			},
		},
	}

	// Create a fake operator client with node statuses that indicate etcd container has been removed
	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&etcd.Spec.StaticPodOperatorSpec,
		&v1.StaticPodOperatorStatus{
			OperatorStatus: v1.OperatorStatus{
				LatestAvailableRevision: 1,
			},
			NodeStatuses: []v1.NodeStatus{
				{
					NodeName:        "master-0",
					CurrentRevision: 1,
				},
			},
		},
		nil,
		nil,
	)

	return args{
		ctx:            ctx,
		operatorClient: fakeOperatorClient,
	}
}

package etcd

import (
	"context"
	"testing"

	v1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
)

func TestRemoveStaticContainer(t *testing.T) {
	ctx := context.Background()
	operatorClient := newFakeOperatorClient()
	kubeClient := fake.NewSimpleClientset()

	err := RemoveStaticContainer(ctx, operatorClient, kubeClient)
	require.NoError(t, err)

	_, status, _, err := operatorClient.GetStaticPodOperatorState()
	require.NoError(t, err)
	require.True(t, v1helpers.IsOperatorConditionTrue(status.Conditions, ceohelpers.OperatorConditionExternalEtcdReadyForTransition))
	require.True(t, v1helpers.IsOperatorConditionTrue(status.Conditions, ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition))

	events, err := kubeClient.CoreV1().Events("openshift-etcd").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	expectedReasons := map[string]bool{
		"EtcdTransitionStarted":                true,
		"EtcdTransitionWaitingForRemoval":      true,
		"EtcdTransitionStaticContainerRemoved": true,
		"EtcdTransitionCompleted":              true,
	}

	require.Len(t, events.Items, len(expectedReasons))
	for _, event := range events.Items {
		require.True(t, expectedReasons[event.Reason], "unexpected event reason: %s", event.Reason)
		require.Equal(t, "Normal", event.Type)
		require.Equal(t, "tnf-setup-runner", event.Source.Component)
		delete(expectedReasons, event.Reason)
	}
	require.Empty(t, expectedReasons, "missing events: %v", expectedReasons)
}

func newFakeOperatorClient() v1helpers.StaticPodOperatorClient {
	return v1helpers.NewFakeStaticPodOperatorClient(
		&v1.StaticPodOperatorSpec{},
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
}

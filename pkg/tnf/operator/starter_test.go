package operator

import (
	"context"
	"errors"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
)

func TestPacemakerLifecycleManagerStartupCondition(t *testing.T) {
	client := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{}, nil, nil)
	ctx := context.Background()

	require.NoError(t, setPacemakerLifecycleManagerStartupCondition(ctx, client, errors.New("informer setup failed")))
	_, status, _, err := client.GetStaticPodOperatorState()
	require.NoError(t, err)
	condition := v1helpers.FindOperatorCondition(status.Conditions, conditionTypePacemakerLifecycleManagerDegraded)
	require.NotNil(t, condition)
	require.Equal(t, operatorv1.ConditionTrue, condition.Status)
	require.Contains(t, condition.Message, "informer setup failed")

	require.NoError(t, setPacemakerLifecycleManagerStartupCondition(ctx, client, nil))
	_, status, _, err = client.GetStaticPodOperatorState()
	require.NoError(t, err)
	condition = v1helpers.FindOperatorCondition(status.Conditions, conditionTypePacemakerLifecycleManagerDegraded)
	require.NotNil(t, condition)
	require.Equal(t, operatorv1.ConditionFalse, condition.Status)
}

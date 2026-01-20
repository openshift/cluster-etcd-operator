package etcdmemberscontroller

import (
	"context"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

var (
	defaultOperatorStatus = func(status *operatorv1.StaticPodOperatorStatus) {}
)

func TestEtcdMembersController_ReportEtcdMembers(t *testing.T) {
	scenarios := []struct {
		name                    string
		etcdMembers             []*etcdserverpb.Member
		memberHealthConfig      *etcdcli.FakeMemberHealth
		infrastructureTopology  configv1.TopologyMode
		operatorSpec            *operatorv1.StaticPodOperatorSpec
		expectedAvailableStatus operatorv1.ConditionStatus
		expectedAvailableReason string
		description             string
		operatorStatus          func(*operatorv1.StaticPodOperatorStatus)
	}{
		{
			name: "HA cluster with quorum - status available",
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
				u.FakeEtcdMemberWithoutServer(3),
			},
			memberHealthConfig: &etcdcli.FakeMemberHealth{
				Healthy:   3,
				Unhealthy: 0,
			},
			infrastructureTopology:  configv1.HighlyAvailableTopologyMode,
			operatorSpec:            &operatorv1.StaticPodOperatorSpec{},
			expectedAvailableStatus: operatorv1.ConditionTrue,
			expectedAvailableReason: "EtcdQuorate",
			description:             "Regular HA cluster with 3/3 healthy nodes should report available",
			operatorStatus:          defaultOperatorStatus,
		},
		{
			name: "HA cluster without quorum - status unavailable",
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
				u.FakeEtcdMemberWithoutServer(3),
			},
			memberHealthConfig: &etcdcli.FakeMemberHealth{
				Healthy:   1,
				Unhealthy: 2,
			},
			infrastructureTopology:  configv1.HighlyAvailableTopologyMode,
			operatorSpec:            &operatorv1.StaticPodOperatorSpec{},
			expectedAvailableStatus: operatorv1.ConditionFalse,
			expectedAvailableReason: "NoQuorum",
			description:             "Regular HA cluster with 1/3 healthy nodes should report unavailable",
			operatorStatus:          defaultOperatorStatus,
		},
		{
			name: "TNF cluster without quorum - status available due to auto recovery",
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			memberHealthConfig: &etcdcli.FakeMemberHealth{
				Healthy:   1,
				Unhealthy: 1,
			},
			infrastructureTopology:  configv1.DualReplicaTopologyMode,
			operatorSpec:            &operatorv1.StaticPodOperatorSpec{},
			expectedAvailableStatus: operatorv1.ConditionTrue,
			expectedAvailableReason: "EtcdQuorate",
			description:             "TNF cluster with 1/2 healthy nodes should report available due to automatic quorum recovery",
			operatorStatus: u.WithConditions(operatorv1.OperatorCondition{
				Type:   ceohelpers.OperatorConditionExternalEtcdHasCompletedTransition,
				Status: operatorv1.ConditionTrue,
			}),
		},
		{
			name: "TNF cluster without quorum - status unavailable because external etcd is not yet ready",
			etcdMembers: []*etcdserverpb.Member{
				u.FakeEtcdMemberWithoutServer(1),
				u.FakeEtcdMemberWithoutServer(2),
			},
			memberHealthConfig: &etcdcli.FakeMemberHealth{
				Healthy:   1,
				Unhealthy: 1,
			},
			infrastructureTopology:  configv1.DualReplicaTopologyMode,
			operatorSpec:            &operatorv1.StaticPodOperatorSpec{},
			expectedAvailableStatus: operatorv1.ConditionFalse,
			expectedAvailableReason: "NoQuorum",
			description:             "TNF cluster with 1/2 healthy nodes should report unavailable when automatic quorum recovery is not yet available",
			operatorStatus:          defaultOperatorStatus,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Create fake etcd client with member health configuration
			fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(
				scenario.etcdMembers,
				etcdcli.WithFakeClusterHealth(scenario.memberHealthConfig),
			)
			require.NoError(t, err)

			// Create fake operator client
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				scenario.operatorSpec,
				u.StaticPodOperatorStatus(scenario.operatorStatus),
				nil,
				nil,
			)

			// Create fake infrastructure lister
			fakeInfrastructureLister := u.FakeInfrastructureLister(t, scenario.infrastructureTopology)

			// Create the controller
			controller := &EtcdMembersController{
				operatorClient:       fakeOperatorClient,
				etcdClient:           fakeEtcdClient,
				infrastructureLister: fakeInfrastructureLister,
			}

			// Create fake event recorder
			eventRecorder := events.NewInMemoryRecorder("test", clock.RealClock{})

			// Execute the reportEtcdMembers method
			err = controller.reportEtcdMembers(context.Background(), eventRecorder)
			require.NoError(t, err)

			// Verify the EtcdMembersAvailable condition
			_, status, _, err := fakeOperatorClient.GetStaticPodOperatorState()
			require.NoError(t, err)

			var availableCondition *operatorv1.OperatorCondition
			for i := range status.Conditions {
				if status.Conditions[i].Type == "EtcdMembersAvailable" {
					availableCondition = &status.Conditions[i]
					break
				}
			}

			require.NotNil(t, availableCondition, "EtcdMembersAvailable condition should be set")
			require.Equal(t, scenario.expectedAvailableStatus, availableCondition.Status,
				"EtcdMembersAvailable status should match expected for %s", scenario.description)
			require.Equal(t, scenario.expectedAvailableReason, availableCondition.Reason,
				"EtcdMembersAvailable reason should match expected for %s", scenario.description)

			t.Logf("✓ %s", scenario.description)
		})
	}
}

func TestEtcdMembersController_ReportEtcdMembers_AdditionalConditions(t *testing.T) {
	// Test that other conditions are also properly set
	etcdMembers := []*etcdserverpb.Member{
		u.FakeEtcdMemberWithoutServer(1),
		u.FakeEtcdMemberWithoutServer(2),
		u.FakeEtcdMemberWithoutServer(3),
	}

	fakeEtcdClient, err := etcdcli.NewFakeEtcdClient(
		etcdMembers,
		etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 3, Unhealthy: 0}),
	)
	require.NoError(t, err)

	fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
		&operatorv1.StaticPodOperatorSpec{},
		u.StaticPodOperatorStatus(),
		nil,
		nil,
	)

	fakeInfrastructureLister := u.FakeInfrastructureLister(t, configv1.HighlyAvailableTopologyMode)

	controller := &EtcdMembersController{
		operatorClient:       fakeOperatorClient,
		etcdClient:           fakeEtcdClient,
		infrastructureLister: fakeInfrastructureLister,
	}

	eventRecorder := events.NewInMemoryRecorder("test", clock.RealClock{})

	err = controller.reportEtcdMembers(context.Background(), eventRecorder)
	require.NoError(t, err)

	_, status, _, err := fakeOperatorClient.GetStaticPodOperatorState()
	require.NoError(t, err)

	// Verify EtcdMembersDegraded condition
	var degradedCondition *operatorv1.OperatorCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == "EtcdMembersDegraded" {
			degradedCondition = &status.Conditions[i]
			break
		}
	}
	require.NotNil(t, degradedCondition)
	require.Equal(t, operatorv1.ConditionFalse, degradedCondition.Status)
	require.Equal(t, "AsExpected", degradedCondition.Reason)

	// Verify EtcdMembersProgressing condition
	var progressingCondition *operatorv1.OperatorCondition
	for i := range status.Conditions {
		if status.Conditions[i].Type == "EtcdMembersProgressing" {
			progressingCondition = &status.Conditions[i]
			break
		}
	}
	require.NotNil(t, progressingCondition)
	require.Equal(t, operatorv1.ConditionFalse, progressingCondition.Status)
	require.Equal(t, "AsExpected", progressingCondition.Reason)
}

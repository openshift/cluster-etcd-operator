package upgradebackupcontroller

import (
	"context"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func Test_ensureRecentBackup(t *testing.T) {
	scenarios := []struct {
		name                    string
		objects                 []runtime.Object
		clusterversionCondition configv1.ClusterOperatorStatusCondition
		ceoCondition            *configv1.ClusterOperatorStatus
		wantBackupStatus        configv1.ConditionStatus
		wantNilBackupCondition  bool
		wantErr                 bool
		wantEventCount          int
	}{
		{
			name: "failed backup pod deleted",
			objects: []runtime.Object{
				u.FakePod("cluster-backup", u.WithPodStatus(corev1.PodFailed), u.WithCreationTimestamp(nowMinusDuration(failedPodBackoffDuration))),
			},
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionFalse,
				Message: fmt.Sprintf("Need RecentBackup"),
			},
			ceoCondition:     &configv1.ClusterOperatorStatus{},
			wantBackupStatus: configv1.ConditionFalse,
			wantEventCount:   1, // pod delete
		},
		{
			name: "failed backup pod created before retry duration elapsed",
			objects: []runtime.Object{
				u.FakePod("cluster-backup", u.WithPodStatus(corev1.PodFailed), u.WithCreationTimestamp(metav1.Now())),
			},
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionFalse,
				Message: fmt.Sprintf("Need RecentBackup"),
			},
			ceoCondition:     &configv1.ClusterOperatorStatus{},
			wantBackupStatus: configv1.ConditionFalse,
			wantEventCount:   0, // skip pod delete
		},
		{
			name: "backup pod pending status",
			objects: []runtime.Object{
				u.FakePod("cluster-backup", u.WithPodStatus(corev1.PodPending)),
			},
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionFalse,
				Message: fmt.Sprintf("Need RecentBackup"),
			},
			ceoCondition:     &configv1.ClusterOperatorStatus{},
			wantBackupStatus: configv1.ConditionUnknown,
		},
		{
			name: "RecentBackup not required invalid type",
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "NotReleaseAccepted",
				Status:  configv1.ConditionFalse,
				Message: fmt.Sprintf("Need RecentBackup"),
			},
			ceoCondition:           &configv1.ClusterOperatorStatus{},
			wantNilBackupCondition: true,
		},
		{
			name: "RecentBackup not required invalid message",
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionFalse,
				Message: fmt.Sprintf("Invalid"),
			},
			ceoCondition:           &configv1.ClusterOperatorStatus{},
			wantNilBackupCondition: true,
		},
		{
			name: "RecentBackup required backup pod created",
			objects: []runtime.Object{
				u.FakePod("etcd-1-master-1", u.WithScheduledNodeName("master-1"), u.WithPodLabels(map[string]string{"app": "etcd"})),
				u.FakeNode("master-1"),
			},
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionFalse,
				Message: fmt.Sprintf("Need RecentBackup"),
			},
			ceoCondition:     &configv1.ClusterOperatorStatus{},
			wantBackupStatus: configv1.ConditionUnknown,
			wantEventCount:   1, // pod created event
		},
		{
			name: "RecentBackup required after two consecutive upgrades",
			objects: []runtime.Object{
				u.FakePod("etcd-1-master-1", u.WithScheduledNodeName("master-1"), u.WithPodLabels(map[string]string{"app": "etcd"})),
				u.FakeNode("master-1"),
			},
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionTrue,
				Message: fmt.Sprintf("Payload loaded version"),
			},
			ceoCondition: &configv1.ClusterOperatorStatus{
				Conditions: []configv1.ClusterOperatorStatusCondition{
					{Type: backupConditionType, Reason: backupSuccess, Message: "UpgradeBackup pre 4.9 located at path "}},
			},
			wantNilBackupCondition: true,
		},
		{
			name: "RecentBackup not required, backup exist for current version",
			objects: []runtime.Object{
				u.FakePod("etcd-1-master-1", u.WithScheduledNodeName("master-1"), u.WithPodLabels(map[string]string{"app": "etcd"})),
				u.FakeNode("master-1"),
			},
			clusterversionCondition: configv1.ClusterOperatorStatusCondition{
				Type:    "ReleaseAccepted",
				Status:  configv1.ConditionTrue,
				Message: fmt.Sprintf("Payload loaded version"),
			},
			ceoCondition: &configv1.ClusterOperatorStatus{
				Conditions: []configv1.ClusterOperatorStatusCondition{
					{Type: backupConditionType, Reason: backupSuccess, Message: "UpgradeBackup pre 4.9.30 located at path "}},
			},
			wantNilBackupCondition: true,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range scenario.objects {
				if err := indexer.Add(obj); err != nil {
					t.Fatal(err)
				}
			}
			fakeKubeClient := fake.NewSimpleClientset(scenario.objects...)
			fakeOperatorClient := v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{
					OperatorSpec: operatorv1.OperatorSpec{
						ManagementState: operatorv1.Managed,
					},
				},
				nil,
				nil,
				nil,
			)

			fakeRecorder := events.NewInMemoryRecorder("test-upgradebackupcontroller")
			clusterVersion := &configv1.ClusterVersion{
				ObjectMeta: metav1.ObjectMeta{Name: "version"},
				Spec:       configv1.ClusterVersionSpec{},
				Status: configv1.ClusterVersionStatus{
					History: []configv1.UpdateHistory{
						{State: configv1.PartialUpdate, Version: "4.10.15"},
						{State: configv1.CompletedUpdate, Version: "4.9.30"},
					},
				},
			}
			fakeEtcdClient, _ := etcdcli.NewFakeEtcdClient(
				[]*etcdserverpb.Member{
					{
						Name: "master-1",
					},
				},
				etcdcli.WithFakeClusterHealth(&etcdcli.FakeMemberHealth{Healthy: 1}),
			)

			clusterVersion.Status.Conditions = append(clusterVersion.Status.Conditions, scenario.clusterversionCondition)
			fakeClusterVersionLister := u.FakeClusterVersionLister(t, clusterVersion)

			c := UpgradeBackupController{
				operatorClient:       fakeOperatorClient,
				kubeClient:           fakeKubeClient,
				etcdClient:           fakeEtcdClient,
				podLister:            v1.NewPodLister(indexer),
				clusterVersionLister: fakeClusterVersionLister,
				targetImagePullSpec:  "quay.io/openshift/cluster-etcd-operator:latest",
			}

			gotCondition, err := c.ensureRecentBackup(context.TODO(), scenario.ceoCondition, fakeRecorder)
			// verify condition
			if gotCondition == nil && !scenario.wantNilBackupCondition {
				t.Fatalf("unexpected nil condition:")
			}
			if gotCondition != nil && scenario.wantNilBackupCondition {
				t.Fatalf("unexpected condition want nil got: %v", gotCondition)
			}

			// verify error
			if err != nil && scenario.wantErr != true {
				t.Fatalf("unexpected error %v", err)
			}
			if err == nil && scenario.wantErr == true {
				t.Fatalf("expected error got nil")
			}

			if scenario.wantEventCount != len(fakeRecorder.Events()) {
				t.Fatalf("unexpected event count: want: %d got: %d", scenario.wantEventCount, len(fakeRecorder.Events()))
			}

			// check condition status
			if gotCondition != nil && gotCondition.Status != scenario.wantBackupStatus {
				t.Fatalf("unexpected operator status, want: %s, got: %s", scenario.wantBackupStatus, gotCondition.Status)
			}
		})
	}
}

func nowMinusDuration(duration time.Duration) metav1.Time {
	return metav1.Time{Time: metav1.Now().Add(-duration)}
}

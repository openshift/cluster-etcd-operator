package pacemaker

import (
	"strings"
	"testing"
	"time"

	pacmkrv1 "github.com/openshift/api/etcd/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// withNodeCondition returns a copy of conditions with the given type overridden to the given status.
func withNodeCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus) []metav1.Condition {
	out := make([]metav1.Condition, len(conditions))
	copy(out, conditions)
	for i := range out {
		if out[i].Type == condType {
			out[i].Status = status
			out[i].LastTransitionTime = metav1.Now()
		}
	}
	return out
}

// withUnhealthyResource returns a copy of node with the given resource's Healthy condition set to False.
func withUnhealthyResource(node pacmkrv1.PacemakerClusterNodeStatus, resourceName pacmkrv1.PacemakerClusterResourceName) pacmkrv1.PacemakerClusterNodeStatus {
	for i := range node.Resources {
		if node.Resources[i].Name == resourceName {
			for j := range node.Resources[i].Conditions {
				if node.Resources[i].Conditions[j].Type == pacmkrv1.ResourceHealthyConditionType {
					node.Resources[i].Conditions[j].Status = metav1.ConditionFalse
				}
			}
		}
	}
	return node
}

// makeNodeWithConditions creates a node status with custom conditions and healthy resources.
func makeNodeWithConditions(name, ip string, conditions []metav1.Condition) pacmkrv1.PacemakerClusterNodeStatus {
	return pacmkrv1.PacemakerClusterNodeStatus{
		Conditions: conditions,
		NodeName:   name,
		Addresses:  []pacmkrv1.PacemakerNodeAddress{{Type: pacmkrv1.PacemakerNodeInternalIP, Address: ip}},
		Resources: []pacmkrv1.PacemakerClusterResourceStatus{
			{Conditions: createHealthyResourceConditions(), Name: pacmkrv1.PacemakerClusterResourceNameKubelet},
			{Conditions: createHealthyResourceConditions(), Name: pacmkrv1.PacemakerClusterResourceNameEtcd},
		},
	}
}

func TestEvaluateHealth(t *testing.T) {
	tests := []struct {
		name           string
		cr             *pacmkrv1.PacemakerCluster
		wantCount      int
		wantSubstrings []string
	}{
		{
			name: "healthy cluster returns no problems",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.111.20"}),
						createHealthyNodeStatus("master-1", []string{"192.168.111.21"}),
					},
				},
			},
			wantCount: 0,
		},
		{
			name: "stale status detected",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.111.20"}),
					},
				},
			},
			wantCount:      1,
			wantSubstrings: []string{"not been updated recently"},
		},
		{
			name: "maintenance mode detected",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createMaintenanceModeClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createHealthyNodeStatus("master-0", []string{"192.168.111.20"}),
					},
				},
			},
			wantCount:      1,
			wantSubstrings: []string{"maintenance mode"},
		},
		{
			name: "offline node detected",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						makeNodeWithConditions("master-0", "192.168.111.20",
							withNodeCondition(createHealthyNodeConditions(), pacmkrv1.NodeOnlineConditionType, metav1.ConditionFalse)),
						createHealthyNodeStatus("master-1", []string{"192.168.111.21"}),
					},
				},
			},
			wantCount:      1,
			wantSubstrings: []string{"master-0 is offline"},
		},
		{
			name: "offline node skips resource and fencing checks",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						// Node is offline AND has unhealthy etcd AND no fencing — should only report offline.
						withUnhealthyResource(
							makeNodeWithConditions("master-0", "192.168.111.20",
								withNodeCondition(
									withNodeCondition(createHealthyNodeConditions(),
										pacmkrv1.NodeOnlineConditionType, metav1.ConditionFalse),
									pacmkrv1.NodeFencingAvailableConditionType, metav1.ConditionFalse)),
							pacmkrv1.PacemakerClusterResourceNameEtcd),
					},
				},
			},
			wantCount:      1,
			wantSubstrings: []string{"master-0 is offline"},
		},
		{
			name: "unhealthy etcd resource detected",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						createUnhealthyNodeStatus("master-0", []string{"192.168.111.20"}, pacmkrv1.PacemakerClusterResourceNameEtcd),
						createHealthyNodeStatus("master-1", []string{"192.168.111.21"}),
					},
				},
			},
			wantCount:      1,
			wantSubstrings: []string{"Etcd resource is unhealthy on node master-0"},
		},
		{
			name: "fencing unavailable detected",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						makeNodeWithConditions("master-0", "192.168.111.20",
							withNodeCondition(createHealthyNodeConditions(), pacmkrv1.NodeFencingAvailableConditionType, metav1.ConditionFalse)),
						createHealthyNodeStatus("master-1", []string{"192.168.111.21"}),
					},
				},
			},
			wantCount:      1,
			wantSubstrings: []string{"Fencing is unavailable on node master-0"},
		},
		{
			name: "multiple problems aggregated",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createMaintenanceModeClusterConditions(),
					Nodes: &[]pacmkrv1.PacemakerClusterNodeStatus{
						makeNodeWithConditions("master-0", "192.168.111.20",
							withNodeCondition(createHealthyNodeConditions(), pacmkrv1.NodeOnlineConditionType, metav1.ConditionFalse)),
						createHealthyNodeStatus("master-1", []string{"192.168.111.21"}),
					},
				},
			},
			wantCount:      2,
			wantSubstrings: []string{"maintenance mode", "master-0 is offline"},
		},
		{
			name: "nil nodes returns no node-level problems",
			cr: &pacmkrv1.PacemakerCluster{
				Status: pacmkrv1.PacemakerClusterStatus{
					LastUpdated: metav1.Now(),
					Conditions:  createHealthyClusterConditions(),
					Nodes:       nil,
				},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := evaluateHealth(tt.cr)
			if len(problems) != tt.wantCount {
				t.Fatalf("expected %d problems, got %d: %v", tt.wantCount, len(problems), problems)
			}
			joined := strings.Join(problems, " | ")
			for _, sub := range tt.wantSubstrings {
				if !strings.Contains(joined, sub) {
					t.Errorf("expected problems to contain %q, got: %v", sub, problems)
				}
			}
		})
	}
}

package etcdenvvar

import (
	"strings"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

func TestGetArbiterNodeEnvVars(test *testing.T) {
	scenarios := []struct {
		name          string
		labels        map[string]string
		nodeName      string
		emptyStatus   bool
		missingNode   bool
		expectedEnv   map[string]string
		expectedError string
	}{
		{
			name:        "empty arbiter label value",
			labels:      map[string]string{"node-role.kubernetes.io/arbiter": ""},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "true"},
		},
		{
			name:        "nonempty arbiter label value",
			labels:      map[string]string{"node-role.kubernetes.io/arbiter": "arbiter"},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "true"},
		},
		{
			name:        "false arbiter label value still means present",
			labels:      map[string]string{"node-role.kubernetes.io/arbiter": "false"},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "true"},
		},
		{
			name:        "escaped node name",
			nodeName:    "arbiter-0.example.com",
			labels:      map[string]string{"node-role.kubernetes.io/arbiter": ""},
			expectedEnv: map[string]string{"NODE_arbiter_0_example_com_IS_ARBITER": "true"},
		},
		{
			name:        "nil labels",
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "false"},
		},
		{
			name:        "empty labels",
			labels:      map[string]string{},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "false"},
		},
		{
			name:        "noncanonical label",
			labels:      map[string]string{"arbiter": "true"},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "false"},
		},
		{
			name:        "master node",
			labels:      map[string]string{"node-role.kubernetes.io/master": ""},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "false"},
		},
		{
			name:        "control-plane node",
			labels:      map[string]string{"node-role.kubernetes.io/control-plane": ""},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "false"},
		},
		{
			name: "both control-plane labels",
			labels: map[string]string{
				"node-role.kubernetes.io/master":        "",
				"node-role.kubernetes.io/control-plane": "",
			},
			expectedEnv: map[string]string{"NODE_node_0_IS_ARBITER": "false"},
		},
		{
			name:          "empty node statuses",
			emptyStatus:   true,
			expectedError: "empty NodeStatuses, can't generate environment for getArbiterNodeEnvVars",
		},
		{
			name:          "missing node",
			missingNode:   true,
			expectedError: "getting arbiter role for node \"node-0\"",
		},
		{
			name: "conflicting master label",
			labels: map[string]string{
				"node-role.kubernetes.io/arbiter": "",
				"node-role.kubernetes.io/master":  "",
			},
			expectedError: "node \"node-0\" has conflicting arbiter and control-plane role labels",
		},
		{
			name: "conflicting control-plane label",
			labels: map[string]string{
				"node-role.kubernetes.io/arbiter":       "false",
				"node-role.kubernetes.io/control-plane": "",
			},
			expectedError: "node \"node-0\" has conflicting arbiter and control-plane role labels",
		},
	}
	for _, scenario := range scenarios {
		test.Run(scenario.name, func(test *testing.T) {
			nodeName := scenario.nodeName
			if nodeName == "" {
				nodeName = "node-0"
			}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			if !scenario.missingNode {
				require.NoError(test, indexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: scenario.labels}}))
			}
			status := operatorv1.StaticPodOperatorStatus{}
			if !scenario.emptyStatus {
				status.NodeStatuses = []operatorv1.NodeStatus{{NodeName: nodeName}}
			}
			envVars, err := getArbiterNodeEnvVars(envVarContext{
				status:           status,
				masterNodeLister: corev1listers.NewNodeLister(indexer),
			})
			if scenario.expectedError != "" {
				require.ErrorContains(test, err, scenario.expectedError)
				require.Nil(test, envVars)
				if scenario.missingNode {
					require.True(test, apierrors.IsNotFound(err))
				}
				return
			}
			require.NoError(test, err)
			require.Equal(test, scenario.expectedEnv, envVars)
		})
	}
}

func TestArbiterNodeEnvVarsPodSubstitution(test *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	status := operatorv1.StaticPodOperatorStatus{}
	nodes := []struct {
		name        string
		labels      map[string]string
		envName     string
		arbiterRole string
	}{
		{
			name:        "master-0.example.com",
			labels:      map[string]string{"node-role.kubernetes.io/master": ""},
			envName:     "NODE_master_0_example_com_IS_ARBITER",
			arbiterRole: "false",
		},
		{
			name:        "master-1.example.com",
			labels:      map[string]string{"node-role.kubernetes.io/control-plane": ""},
			envName:     "NODE_master_1_example_com_IS_ARBITER",
			arbiterRole: "false",
		},
		{
			name:        "arbiter-0.example.com",
			labels:      map[string]string{"node-role.kubernetes.io/arbiter": ""},
			envName:     "NODE_arbiter_0_example_com_IS_ARBITER",
			arbiterRole: "true",
		},
	}
	expectedEnv := map[string]string{}
	for _, node := range nodes {
		require.NoError(test, indexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.name, Labels: node.labels}}))
		status.NodeStatuses = append(status.NodeStatuses, operatorv1.NodeStatus{NodeName: node.name})
		expectedEnv[node.envName] = node.arbiterRole
	}
	envVars, err := getArbiterNodeEnvVars(envVarContext{
		status:           status,
		masterNodeLister: corev1listers.NewNodeLister(indexer),
	})
	require.NoError(test, err)
	require.Equal(test, expectedEnv, envVars)
	substitution, err := ceohelpers.GetPodSubstitution(
		&operatorv1.StaticPodOperatorSpec{}, "etcd:test", "operator:test", envVars, &operatorv1.Etcd{}, false)
	require.NoError(test, err)
	rendered, err := ceohelpers.RenderTemplate("etcd/pod.gotpl.yaml", substitution)
	require.NoError(test, err)
	require.Contains(test, rendered, "NODE_NODE_ENVVAR_NAME_IS_ARBITER")
	for _, node := range nodes {
		test.Run(node.name, func(test *testing.T) {
			escapedName := strings.ReplaceAll(strings.ReplaceAll(node.name, "-", "_"), ".", "_")
			installed := strings.ReplaceAll(rendered, "NODE_ENVVAR_NAME", escapedName)
			require.NotContains(test, installed, "NODE_ENVVAR_NAME")
			pod := &corev1.Pod{}
			require.NoError(test, yaml.Unmarshal([]byte(installed), pod))
			var resourcesCopy *corev1.Container
			for containerIndex := range pod.Spec.InitContainers {
				if pod.Spec.InitContainers[containerIndex].Name == "etcd-resources-copy" {
					require.Nil(test, resourcesCopy)
					resourcesCopy = &pod.Spec.InitContainers[containerIndex]
				}
			}
			require.NotNil(test, resourcesCopy)
			copyEnv := map[string]string{}
			for _, envVar := range resourcesCopy.Env {
				require.Nil(test, envVar.ValueFrom)
				copyEnv[envVar.Name] = envVar.Value
			}
			require.Equal(test, expectedEnv, copyEnv)
			require.Equal(test, node.arbiterRole, copyEnv[node.envName])
			require.Len(test, resourcesCopy.Command, 3)
			require.Equal(test, []string{"/bin/sh", "-c"}, resourcesCopy.Command[:2])
			require.Contains(test, resourcesCopy.Command[2], `node_is_arbiter="${`+node.envName+`:-}"`)
		})
	}
}

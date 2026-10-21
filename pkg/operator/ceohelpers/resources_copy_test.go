package ceohelpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestResourcesCopyInstallsBackupWithoutRoleGate(test *testing.T) {
	test.Parallel()
	environment := map[string]string{"NODE_fixture_IP": "127.0.0.2"}
	substitution, err := GetPodSubstitution(&operatorv1.StaticPodOperatorSpec{}, "etcd-image", "operator-image", environment, &operatorv1.Etcd{}, false)
	require.NoError(test, err)
	rendered, err := RenderTemplate("etcd/pod.gotpl.yaml", substitution)
	require.NoError(test, err)
	var pod corev1.Pod
	require.NoError(test, yaml.Unmarshal([]byte(rendered), &pod))
	var command string
	for _, container := range pod.Spec.InitContainers {
		if container.Name == "etcd-resources-copy" {
			require.Len(test, container.Command, 3)
			command = container.Command[2]
			require.Empty(test, container.Env)
		}
	}
	require.NotEmpty(test, command)
	require.NotContains(test, command, "IS_ARBITER")
	root := test.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	require.NoError(test, os.MkdirAll(source, 0700))
	require.NoError(test, os.MkdirAll(destination, 0700))
	for _, script := range []string{"cluster-backup.sh", "cluster-restore.sh", "quorum-restore.sh"} {
		require.NoError(test, os.WriteFile(filepath.Join(source, script), bindata.MustAsset("etcd/"+script), 0700))
	}
	command = strings.ReplaceAll(command, "/etc/kubernetes/static-pod-certs/configmaps/etcd-scripts", source)
	command = strings.ReplaceAll(command, "/usr/local/bin", destination)
	output, err := exec.Command("bash", "-c", command).CombinedOutput()
	require.NoError(test, err, string(output))
	backup, err := os.ReadFile(filepath.Join(destination, "cluster-backup.sh"))
	require.NoError(test, err)
	require.Equal(test, bindata.MustAsset("etcd/cluster-backup.sh"), backup)
}

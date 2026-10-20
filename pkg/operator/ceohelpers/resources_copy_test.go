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

func renderedResourcesCopy(test *testing.T, nodeName string, role string) corev1.Container {
	test.Helper()
	environment := map[string]string{
		"NODE_cp_0_example_IS_ARBITER":      "false",
		"NODE_cp_1_example_IS_ARBITER":      "false",
		"NODE_arbiter_0_example_IS_ARBITER": "true",
	}
	key := "NODE_" + strings.NewReplacer("-", "_", ".", "_").Replace(nodeName) + "_IS_ARBITER"
	if role == "missing" {
		delete(environment, key)
	} else {
		environment[key] = role
	}
	substitution, err := GetPodSubstitution(&operatorv1.StaticPodOperatorSpec{}, "etcd-image", "operator-image", environment, &operatorv1.Etcd{}, false)
	require.NoError(test, err)
	rendered, err := RenderTemplate("etcd/pod.gotpl.yaml", substitution)
	require.NoError(test, err)
	rendered = strings.ReplaceAll(rendered, "REVISION", "7")
	rendered = strings.ReplaceAll(rendered, "NODE_NAME", nodeName)
	rendered = strings.ReplaceAll(rendered, "NODE_ENVVAR_NAME", strings.NewReplacer("-", "_", ".", "_").Replace(nodeName))
	var pod corev1.Pod
	require.NoError(test, yaml.Unmarshal([]byte(rendered), &pod))
	for _, container := range pod.Spec.InitContainers {
		if container.Name == "etcd-resources-copy" {
			require.NotContains(test, container.Command[2], "NODE_ENVVAR_NAME")
			return container
		}
	}
	test.Fatal("rendered pod is missing resources-copy")
	return corev1.Container{}
}

func TestResourcesCopyPerNodeDeployment(test *testing.T) {
	test.Parallel()
	scenarios := []struct {
		name, node, role, stale, failure string
		wantError                        bool
	}{
		{name: "first CP without API manifest", node: "cp-0.example", role: "false"},
		{name: "second CP without API manifest", node: "cp-1.example", role: "false"},
		{name: "fresh arbiter", node: "arbiter-0.example", role: "true"},
		{name: "marked stale arbiter", node: "arbiter-0.example", role: "true", stale: "marked"},
		{name: "unmarked stale arbiter", node: "arbiter-0.example", role: "true", stale: "unmarked"},
		{name: "symlink stale arbiter", node: "arbiter-0.example", role: "true", stale: "symlink"},
		{name: "dangling symlink arbiter", node: "arbiter-0.example", role: "true", stale: "dangling"},
		{name: "CP refresh", node: "cp-0.example", role: "false", stale: "unmarked"},
		{name: "directory is not recursively deleted", node: "arbiter-0.example", role: "true", stale: "directory", wantError: true},
		{name: "missing flag", node: "arbiter-0.example", role: "missing", stale: "marked", wantError: true},
		{name: "empty flag", node: "arbiter-0.example", role: "", stale: "marked", wantError: true},
		{name: "invalid flag", node: "arbiter-0.example", role: "yes", stale: "marked", wantError: true},
		{name: "delete failure", node: "arbiter-0.example", role: "true", stale: "unmarked", failure: "rm", wantError: true},
		{name: "copy failure", node: "cp-0.example", role: "false", failure: "cp", wantError: true},
	}
	for _, scenario := range scenarios {
		test.Run(scenario.name, func(test *testing.T) {
			test.Parallel()
			root := test.TempDir()
			source := filepath.Join(root, "source")
			destination := filepath.Join(root, "destination")
			require.NoError(test, os.Mkdir(source, 0755))
			require.NoError(test, os.Mkdir(destination, 0755))
			backup := bindata.MustAsset("etcd/cluster-backup.sh")
			other := []byte("### Created by cluster-etcd-operator\nother script\n")
			require.NoError(test, os.WriteFile(filepath.Join(source, "cluster-backup.sh"), backup, 0755))
			require.NoError(test, os.WriteFile(filepath.Join(source, "other.sh"), other, 0755))
			sentinel := filepath.Join(destination, "unrelated")
			require.NoError(test, os.WriteFile(sentinel, []byte("keep"), 0644))
			managed := filepath.Join(destination, "old-managed.sh")
			require.NoError(test, os.WriteFile(managed, other, 0755))
			target := filepath.Join(root, "symlink-target")
			require.NoError(test, os.WriteFile(target, []byte("target stays"), 0644))
			backupPath := filepath.Join(destination, "cluster-backup.sh")
			switch scenario.stale {
			case "marked":
				require.NoError(test, os.WriteFile(backupPath, other, 0755))
			case "unmarked":
				require.NoError(test, os.WriteFile(backupPath, []byte("stale backup"), 0755))
			case "symlink":
				require.NoError(test, os.Symlink(target, backupPath))
			case "dangling":
				require.NoError(test, os.Symlink(filepath.Join(root, "absent"), backupPath))
			case "directory":
				require.NoError(test, os.Mkdir(backupPath, 0755))
				require.NoError(test, os.WriteFile(filepath.Join(backupPath, "keep"), []byte("keep"), 0644))
			}
			container := renderedResourcesCopy(test, scenario.node, scenario.role)
			command := strings.NewReplacer("/usr/local/bin", destination, "/etc/kubernetes/static-pod-certs/configmaps/etcd-scripts", source).Replace(container.Command[2])
			syntax := exec.Command("bash", "-n")
			syntax.Stdin = strings.NewReader(command)
			require.NoError(test, syntax.Run())
			if scenario.failure == "rm" {
				command = "rm() { if [ \"${2:-}\" = \"" + backupPath + "\" ]; then return 19; fi; command rm \"$@\"; };\n" + command
			} else if scenario.failure == "cp" {
				command = "cp() { return 19; };\n" + command
			}
			for execution := 0; execution < 2; execution++ {
				process := exec.Command("bash", "-c", command)
				process.Env = os.Environ()
				for _, variable := range container.Env {
					process.Env = append(process.Env, variable.Name+"="+variable.Value)
				}
				output, err := process.CombinedOutput()
				if scenario.wantError {
					require.Error(test, err, string(output))
					if scenario.role != "true" && scenario.role != "false" {
						require.Contains(test, string(output), "Invalid or missing arbiter role")
						require.FileExists(test, managed)
						require.FileExists(test, backupPath)
					}
					if scenario.stale == "directory" {
						require.FileExists(test, filepath.Join(backupPath, "keep"))
					}
					if scenario.failure == "rm" {
						require.FileExists(test, backupPath)
					}
				} else {
					require.NoError(test, err, string(output))
					if scenario.role == "true" {
						_, err := os.Lstat(backupPath)
						require.True(test, os.IsNotExist(err), "backup entry should be absent: %v", err)
					} else {
						actual, err := os.ReadFile(backupPath)
						require.NoError(test, err)
						require.Equal(test, backup, actual)
						info, err := os.Stat(backupPath)
						require.NoError(test, err)
						require.Equal(test, os.FileMode(0755), info.Mode().Perm())
					}
					actual, err := os.ReadFile(filepath.Join(destination, "other.sh"))
					require.NoError(test, err)
					require.Equal(test, other, actual)
					require.NoFileExists(test, managed)
				}
				actual, err := os.ReadFile(sentinel)
				require.NoError(test, err)
				require.Equal(test, "keep", string(actual))
				actual, err = os.ReadFile(target)
				require.NoError(test, err)
				require.Equal(test, "target stays", string(actual))
				actual, err = os.ReadFile(filepath.Join(source, "cluster-backup.sh"))
				require.NoError(test, err)
				require.Equal(test, backup, actual)
				actual, err = os.ReadFile(filepath.Join(source, "other.sh"))
				require.NoError(test, err)
				require.Equal(test, other, actual)
			}
		})
	}
}

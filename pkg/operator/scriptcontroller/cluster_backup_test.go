package scriptcontroller

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/stretchr/testify/require"
)

const controlPlaneBackupGuidance = "Run this script on a control-plane node with a running kube-apiserver."

var clusterBackupResources = []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"}

type clusterBackupFixture struct {
	root       string
	archive    string
	kubeconfig string
	forced     bool
	progress   string
}

func newClusterBackupFixture(test *testing.T) *clusterBackupFixture {
	test.Helper()
	root := test.TempDir()
	fixture := &clusterBackupFixture{
		root:       root,
		archive:    filepath.Join(root, "backup.tar.gz"),
		kubeconfig: filepath.Join(root, "kubeconfig"),
		progress:   "False",
	}
	require.NoError(test, os.MkdirAll(filepath.Join(root, "manifests"), 0700))
	require.NoError(test, os.WriteFile(fixture.kubeconfig, nil, 0600))
	for _, resource := range clusterBackupResources {
		resourceDir := filepath.Join(root, "static-pod-resources", resource+"-pod-1")
		require.NoError(test, os.MkdirAll(resourceDir, 0700))
		require.NoError(test, os.WriteFile(filepath.Join(resourceDir, "fixture"), []byte(resource), 0600))
		require.NoError(test, os.WriteFile(fixture.manifest(resource), []byte(resourceDir+"\n"), 0600))
	}
	return fixture
}

func (fixture *clusterBackupFixture) manifest(resource string) string {
	return filepath.Join(fixture.root, "manifests", resource+"-pod.yaml")
}

func clusterBackupFunction(test *testing.T, script, name string) string {
	test.Helper()
	declaration := "function " + name + " {\n"
	require.Equal(test, 1, strings.Count(script, declaration))
	_, body, found := strings.Cut(script, declaration)
	require.True(test, found)
	body, _, found = strings.Cut(body, "\n}\n")
	require.True(test, found, "missing function boundary for %s", name)
	return declaration + body + "\n}\n"
}

func (fixture *clusterBackupFixture) run(test *testing.T) (string, error) {
	test.Helper()
	script := string(bindata.MustAsset("etcd/cluster-backup.sh"))
	functions := clusterBackupFunction(test, script, "check_if_operator_is_progressing") +
		clusterBackupFunction(test, script, "backup_latest_kube_static_resources")
	functions = strings.ReplaceAll(functions, "/etc/kubernetes", fixture.root)
	harness := `set -o errexit
set -o pipefail
set -o errtrace
function oc {
  printf '%s\n' "$3" >> "${CONFIG_FILE_DIR}/operator-calls"
  printf '%s' "$PROGRESSING"
}
function tar {
  printf 'tar\n' >> "${CONFIG_FILE_DIR}/tar-calls"
  command tar "$@"
}
` + functions + `backup_latest_kube_static_resources "$1"`
	command := exec.Command("bash", "-c", harness, "cluster-backup-test", fixture.archive)
	isDirty := ""
	if fixture.forced {
		isDirty = "__POSSIBLY_DIRTY__"
	}
	command.Env = append(os.Environ(),
		"CONFIG_FILE_DIR="+fixture.root,
		"KUBECONFIG="+fixture.kubeconfig,
		"IS_DIRTY="+isDirty,
		"PROGRESSING="+fixture.progress,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (fixture *clusterBackupFixture) requireFailure(test *testing.T, output string, err error, diagnostic string) {
	test.Helper()
	var exitError *exec.ExitError
	require.ErrorAs(test, err, &exitError, output)
	require.Equal(test, 1, exitError.ExitCode(), output)
	require.Contains(test, output, diagnostic)
	require.NoFileExists(test, fixture.archive)
	require.NoFileExists(test, filepath.Join(fixture.root, "tar-calls"))
}

func TestClusterBackupMissingManifest(test *testing.T) {
	for _, resource := range clusterBackupResources {
		for _, forced := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/forced=%t", resource, forced), func(test *testing.T) {
				fixture := newClusterBackupFixture(test)
				fixture.forced = forced
				require.NoError(test, os.Remove(fixture.manifest(resource)))
				output, err := fixture.run(test)
				diagnostic := fmt.Sprintf("error finding manifests for the %s pod. please check if it is running.\n", resource)
				fixture.requireFailure(test, output, err, diagnostic)
				if resource == "kube-apiserver" {
					require.Equal(test, diagnostic+controlPlaneBackupGuidance+"\n", output)
				} else {
					require.NotContains(test, output, controlPlaneBackupGuidance)
				}
			})
		}
	}
}

func TestClusterBackupMissingResourceReference(test *testing.T) {
	for _, resource := range clusterBackupResources {
		test.Run(resource, func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			fixture.forced = true
			require.NoError(test, os.WriteFile(fixture.manifest(resource), []byte("no revision reference\n"), 0600))
			output, err := fixture.run(test)
			fixture.requireFailure(test, output, err,
				fmt.Sprintf("error finding static-pod-resources for the %s pod. please check if it is running.\n", resource))
			require.NotContains(test, output, controlPlaneBackupGuidance)
		})
	}
}

func TestClusterBackupArchive(test *testing.T) {
	for _, forced := range []bool{false, true} {
		test.Run(fmt.Sprintf("forced=%t", forced), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			fixture.forced = forced
			output, err := fixture.run(test)
			require.NoError(test, err, output)
			require.NotContains(test, output, controlPlaneBackupGuidance)
			archiveInfo, err := os.Stat(fixture.archive)
			require.NoError(test, err)
			require.Equal(test, os.FileMode(0600), archiveInfo.Mode().Perm())
			contents, err := exec.Command("tar", "-tzf", fixture.archive).CombinedOutput()
			require.NoError(test, err, string(contents))
			expectedEntries := []string{}
			for _, resource := range clusterBackupResources {
				resourceDir := "static-pod-resources/" + resource + "-pod-1/"
				expectedEntries = append(expectedEntries, resourceDir, resourceDir+"fixture")
			}
			require.Equal(test, expectedEntries, strings.Split(strings.TrimSpace(string(contents)), "\n"))
			if forced {
				require.NoFileExists(test, filepath.Join(fixture.root, "operator-calls"))
			} else {
				calls, err := os.ReadFile(filepath.Join(fixture.root, "operator-calls"))
				require.NoError(test, err)
				require.Equal(test, strings.Join(clusterBackupResources, "\n")+"\n", string(calls))
			}
		})
	}
}

func TestClusterBackupOperatorChecks(test *testing.T) {
	for _, testCase := range []struct {
		name              string
		progress          string
		missingKubeconfig bool
		diagnostic        string
	}{
		{
			name:       "progressing",
			progress:   "True",
			diagnostic: "Currently the kube-apiserver operator is progressing. A reliable backup requires that a rollout is not in progress.  Aborting!",
		},
		{
			name:       "unavailable status",
			diagnostic: "Could not find the status of the kube-apiserver. Check if the API server is running. Pass the --force flag to skip checks.",
		},
		{
			name:              "missing kubeconfig",
			missingKubeconfig: true,
			diagnostic:        "Valid kubeconfig is not found in kube-apiserver-certs. Exiting!",
		},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			fixture.progress = testCase.progress
			if testCase.missingKubeconfig {
				require.NoError(test, os.Remove(fixture.kubeconfig))
			}
			output, err := fixture.run(test)
			fixture.requireFailure(test, output, err, testCase.diagnostic+"\n")
			require.NotContains(test, output, controlPlaneBackupGuidance)
			fixture.forced = true
			output, err = fixture.run(test)
			require.NoError(test, err, output)
		})
	}
}

func TestClusterBackupSyntax(test *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(string(bindata.MustAsset("etcd/cluster-backup.sh")))
	output, err := command.CombinedOutput()
	require.NoError(test, err, string(output))
}

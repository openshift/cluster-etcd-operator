package scriptcontroller

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

var clusterBackupResources = []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "etcd"}

type clusterBackupFixture struct {
	root string
	out  string
	env  []string
}

func backupFixtureWrite(test *testing.T, path, content string, mode os.FileMode) {
	test.Helper()
	require.NoError(test, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(test, os.WriteFile(path, []byte(content), mode))
}

func backupFixtureManifest(paths ...string) string {
	manifest := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: fixture\nspec:\n  containers:\n  - name: fixture\n    image: fixture\n  volumes:\n"
	for index, path := range paths {
		manifest += fmt.Sprintf("  - name: fixture-%d\n    hostPath:\n      path: %q\n", index, path)
	}
	return manifest
}

func TestClusterBackupManifestDecoderStub(test *testing.T) {
	if os.Getenv("CLUSTER_BACKUP_MANIFEST_DECODER") != "1" {
		return
	}
	manifest, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil {
		os.Exit(2)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(manifest, &pod); err != nil || pod.Kind != "Pod" || pod.APIVersion != "v1" {
		os.Exit(2)
	}
	fmt.Printf("%s\n%s\n", pod.APIVersion, pod.Kind)
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			fmt.Println(volume.HostPath.Path)
		}
	}
	os.Exit(0)
}

func newClusterBackupFixture(test *testing.T) *clusterBackupFixture {
	test.Helper()
	fixture := &clusterBackupFixture{root: test.TempDir()}
	fixture.out = filepath.Join(fixture.root, "backup with spaces")
	fixture.env = []string{"PROGRESSING=False"}
	executable, err := os.Executable()
	require.NoError(test, err)
	fixture.env = append(fixture.env, "FIXTURE_TEST_BINARY="+executable)
	for _, resource := range clusterBackupResources {
		revision := filepath.Join(fixture.root, "static-pod-resources", resource+"-pod-1")
		manifest := backupFixtureManifest(revision)
		backupFixtureWrite(test, fixture.manifest(resource), manifest, 0600)
		backupFixtureWrite(test, filepath.Join(revision, resource+"-pod.yaml"), manifest, 0600)
		backupFixtureWrite(test, filepath.Join(revision, "fixture"), resource, 0600)
		backupFixtureWrite(test, filepath.Join(fixture.root, "static-pod-resources", resource+"-pod-99", "not-active"), "unused", 0600)
	}
	dependencies := filepath.Join(fixture.root, "static-pod-resources/etcd-certs/configmaps/etcd-scripts")
	helper := strings.ReplaceAll(string(bindata.MustAsset("etcd/etcd-common-tools")), "/etc/kubernetes", fixture.root)
	backupFixtureWrite(test, filepath.Join(dependencies, "etcd-common-tools"), helper, 0600)
	backupFixtureWrite(test, filepath.Join(dependencies, "etcd.env"), fmt.Sprintf(`export ETCDCTL_CACERT=%q
export ETCDCTL_CERT=%q
export ETCDCTL_KEY=%q
export NODE_NODE_ENVVAR_NAME_IP=127.0.0.2
export ETCD_IMAGE=fixture-image
`, fixture.root+"/static-pod-certs/ca", fixture.root+"/static-pod-certs/cert", fixture.root+"/static-pod-certs/key"), 0600)
	for _, name := range []string{"ca", "cert", "key"} {
		backupFixtureWrite(test, filepath.Join(fixture.root, "static-pod-resources/etcd-certs", name), "", 0600)
	}
	backupFixtureWrite(test, fixture.kubeconfig(), "", 0600)
	stubs := map[string]string{
		"oc": `if [ "$1" = --kubeconfig=/dev/null ]; then
  [ "$2" = --server=https://127.0.0.1:1 ] && [ "$3 $4 $5 $6 $8" = 'set env --local -f -o' ] || exit 8
  if [ "${OC_MANIFEST_FAILURE:-}" = true ] && [[ "$7" = *kube-apiserver-pod.yaml ]]; then exit 2; fi
  if [ -n "${REAL_OC:-}" ]; then exec "$REAL_OC" "$@"; fi
  CLUSTER_BACKUP_MANIFEST_DECODER=1 GORACE=atexit_sleep_ms=0 exec "$FIXTURE_TEST_BINARY" -test.run='^TestClusterBackupManifestDecoderStub$' -- "$7"
fi
printf '%s\n' "$3" >> "$FIXTURE_ROOT/operator-calls"
[ "$KUBECONFIG" = "$FIXTURE_ROOT/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/localhost.kubeconfig" ] || exit 8
[ "${OC_FAILURE:-}" != true ] || exit 9
if [ "${PROGRESSING_OPERATOR:-}" = "$3" ]; then printf True; exit 0; fi
printf '%s' "$PROGRESSING"
`,
		"podman": `case "$1" in
create) echo fixture-container ;;
mount) echo "$FIXTURE_ROOT/image" ;;
rm) exit 0 ;;
*) exit 9 ;;
esac
`,
		"umount": `exit 0
`,
		"date": `echo 2026-01-01_000000
`,
	}
	for name, body := range stubs {
		backupFixtureWrite(test, filepath.Join(fixture.root, "bin", name), "#!/bin/bash\n"+body, 0700)
	}
	backupFixtureWrite(test, filepath.Join(fixture.root, "image/bin/etcdctl"), `#!/bin/bash
if [ "$1" = version ]; then exit 0; fi
if [ "$2" = save ]; then
  [ "$ETCDCTL_ENDPOINTS" = https://127.0.0.2:2379 ] || exit 8
  for name in "$ETCDCTL_CACERT" "$ETCDCTL_CERT" "$ETCDCTL_KEY"; do
    [ -f "$name" ] || exit 8
    [[ "$name" = "$FIXTURE_ROOT"/static-pod-resources/etcd-certs/* ]] || exit 8
  done
  echo snapshot > "$3"
  echo save >> "$FIXTURE_ROOT/snapshot-calls"
  if [ "${EMPTY_SNAPSHOT:-}" = true ]; then truncate -s 0 "$3"; fi
  exit "${SNAPSHOT_EXIT:-0}"
fi
if [ "$2" = status ]; then
  echo status >> "$FIXTURE_ROOT/snapshot-calls"
  exit "${STATUS_EXIT:-0}"
fi
exit 9
`, 0700)
	tool, err := os.ReadFile(filepath.Join(fixture.root, "image/bin/etcdctl"))
	require.NoError(test, err)
	backupFixtureWrite(test, filepath.Join(fixture.root, "image/bin/etcdutl"), string(tool), 0700)
	for _, name := range []string{"etcdctl", "etcdutl"} {
		backupFixtureWrite(test, filepath.Join(fixture.root, "host-bin", name), "#!/bin/bash\necho unexpected-host-tool > \"$FIXTURE_ROOT/host-tools-used\"\nexit 99\n", 0700)
	}
	return fixture
}

func (fixture *clusterBackupFixture) manifest(resource string) string {
	return filepath.Join(fixture.root, "manifests", resource+"-pod.yaml")
}

func (fixture *clusterBackupFixture) kubeconfig() string {
	return filepath.Join(fixture.root, "static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/localhost.kubeconfig")
}

func (fixture *clusterBackupFixture) run(test *testing.T, args ...string) (string, error) {
	test.Helper()
	script := strings.ReplaceAll(string(bindata.MustAsset("etcd/cluster-backup.sh")), "/etc/kubernetes", fixture.root)
	script = strings.ReplaceAll(script, "$EUID", "${FIXTURE_EUID:-0}")
	entrypoint := filepath.Join(fixture.root, "cluster-backup.sh")
	backupFixtureWrite(test, entrypoint, script, 0700)
	command := exec.Command("bash", append([]string{entrypoint}, args...)...)
	command.Dir = fixture.root
	command.Env = append(os.Environ(), "PATH="+strings.Join([]string{filepath.Join(fixture.root, "bin"), filepath.Join(fixture.root, "static-pod-resources/bin"), filepath.Join(fixture.root, "host-bin"), os.Getenv("PATH")}, ":"), "FIXTURE_ROOT="+fixture.root)
	command.Env = append(command.Env, fixture.env...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (fixture *clusterBackupFixture) removeOptional(test *testing.T) {
	test.Helper()
	for _, resource := range clusterBackupResources[:3] {
		require.NoError(test, os.Remove(fixture.manifest(resource)))
	}
}

func (fixture *clusterBackupFixture) requireNoArtifacts(test *testing.T) {
	test.Helper()
	entries, err := os.ReadDir(fixture.out)
	if os.IsNotExist(err) {
		return
	}
	require.NoError(test, err)
	require.Empty(test, entries)
}

func backupArchiveContents(test *testing.T, filename string) map[string]string {
	test.Helper()
	file, err := os.Open(filename)
	require.NoError(test, err)
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	require.NoError(test, err)
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	contents := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(test, err)
		data, err := io.ReadAll(reader)
		require.NoError(test, err)
		contents[header.Name] = string(data)
	}
	return contents
}

func TestClusterBackupFullEntryPoint(test *testing.T) {
	test.Parallel()
	for _, forced := range []bool{false, true} {
		test.Run(fmt.Sprintf("force=%t", forced), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			args := []string{fixture.out}
			suffix := ""
			if forced {
				args = append([]string{"--force"}, args...)
				suffix = "__POSSIBLY_DIRTY__"
			}
			output, err := fixture.run(test, args...)
			require.NoError(test, err, output)
			require.Contains(test, output, "snapshot db and kube resources are successfully saved")
			require.NotContains(test, output, "PARTIAL")
			archive := filepath.Join(fixture.out, "static_kuberesources_2026-01-01_000000"+suffix+".tar.gz")
			contents := backupArchiveContents(test, archive)
			require.Len(test, contents, 12)
			for _, resource := range clusterBackupResources {
				require.Equal(test, resource, contents["static-pod-resources/"+resource+"-pod-1/fixture"])
			}
			snapshot := filepath.Join(fixture.out, "snapshot_2026-01-01_000000"+suffix+".db")
			for _, artifact := range []string{archive, snapshot} {
				info, err := os.Stat(artifact)
				require.NoError(test, err)
				require.Equal(test, os.FileMode(0600), info.Mode().Perm())
			}
			calls, err := os.ReadFile(filepath.Join(fixture.root, "snapshot-calls"))
			require.NoError(test, err)
			require.Equal(test, "save\nstatus\n", string(calls))
			require.NoFileExists(test, filepath.Join(fixture.root, "host-tools-used"))
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

func TestClusterBackupPartialEntryPoint(test *testing.T) {
	test.Parallel()
	for _, available := range []int{0, 1, 3} {
		test.Run(fmt.Sprintf("optional=%d", available), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			for _, resource := range clusterBackupResources[available:3] {
				require.NoError(test, os.Remove(fixture.manifest(resource)))
			}
			require.NoError(test, os.Remove(fixture.kubeconfig()))
			for invocation := 0; invocation < 2; invocation++ {
				output, err := fixture.run(test, "--available-static-resources", "--force", fixture.out)
				require.NoError(test, err, output)
				require.Contains(test, output, "WARNING: PARTIAL")
				require.NotContains(test, output, "snapshot db and kube resources are successfully saved")
				for _, resource := range clusterBackupResources[available:3] {
					require.Contains(test, output, "OMITTED "+resource+": local manifest is absent")
				}
			}
			archives, err := filepath.Glob(filepath.Join(fixture.out, "partial_static_kuberesources_*.tar.gz"))
			require.NoError(test, err)
			require.Len(test, archives, 2)
			snapshots, err := filepath.Glob(filepath.Join(fixture.out, "partial_snapshot_*.db"))
			require.NoError(test, err)
			require.Len(test, snapshots, 2)
			for _, archive := range archives {
				contents := backupArchiveContents(test, archive)
				require.Len(test, contents, 3*(available+1)+1)
				require.Contains(test, contents["PARTIAL_BACKUP.txt"], "NOT qualified for full control-plane recovery")
				require.Contains(test, contents["PARTIAL_BACKUP.txt"], "INCLUDED etcd:")
				for _, resource := range clusterBackupResources[available:3] {
					require.Contains(test, contents["PARTIAL_BACKUP.txt"], "OMITTED "+resource)
				}
			}
			require.NoFileExists(test, filepath.Join(fixture.root, "operator-calls"))
			for _, pattern := range []string{"static_kuberesources*.tar.gz", "snapshot*.db", ".cluster-backup*"} {
				matches, err := filepath.Glob(filepath.Join(fixture.out, pattern))
				require.NoError(test, err)
				require.Empty(test, matches)
			}
		})
	}
}

func TestClusterBackupEntryPointFailures(test *testing.T) {
	test.Parallel()
	for _, scenario := range []struct {
		name       string
		partial    bool
		configure  func(*testing.T, *clusterBackupFixture)
		diagnostic string
	}{
		{name: "full missing optional", configure: func(test *testing.T, fixture *clusterBackupFixture) {
			require.NoError(test, os.Remove(fixture.manifest("kube-apiserver")))
		}, diagnostic: "error finding manifests for the kube-apiserver"},
		{name: "missing etcd manifest", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			require.NoError(test, os.Remove(fixture.manifest("etcd")))
		}, diagnostic: "Required etcd manifest is missing"},
		{name: "missing etcd revision", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			require.NoError(test, os.RemoveAll(filepath.Join(fixture.root, "static-pod-resources/etcd-pod-1")))
		}, diagnostic: "Required etcd revision is missing"},
		{name: "malformed optional", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, fixture.manifest("kube-apiserver"), "not a pod manifest", 0600)
		}, diagnostic: "Invalid or unreadable Pod manifest"},
		{name: "comment-only etcd manifest", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, fixture.manifest("etcd"), "# "+fixture.root+"/static-pod-resources/etcd-pod-1\n", 0600)
		}, diagnostic: "Invalid or unreadable Pod manifest"},
		{name: "malformed revision manifest", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, filepath.Join(fixture.root, "static-pod-resources/kube-apiserver-pod-1/kube-apiserver-pod.yaml"), "invalid: [\n", 0600)
		}, diagnostic: "Invalid or unreadable Pod manifest"},
		{name: "mismatched revision manifest", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, filepath.Join(fixture.root, "static-pod-resources/kube-apiserver-pod-1/kube-apiserver-pod.yaml"), backupFixtureManifest(fixture.root+"/static-pod-resources/kube-apiserver-pod-99"), 0600)
		}, diagnostic: "Installed and revision manifests disagree"},
		{name: "invalid revision", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, fixture.manifest("kube-apiserver"), backupFixtureManifest(fixture.root+"/static-pod-resources/kube-apiserver-pod-invalid"), 0600)
		}, diagnostic: "Invalid or unreadable revision reference"},
		{name: "ambiguous revision", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, fixture.manifest("kube-apiserver"), backupFixtureManifest(fixture.root+"/static-pod-resources/kube-apiserver-pod-1", fixture.root+"/static-pod-resources/kube-apiserver-pod-99"), 0600)
		}, diagnostic: "Invalid or unreadable revision reference"},
		{name: "traversal revision", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, fixture.manifest("etcd"), backupFixtureManifest(fixture.root+"/static-pod-resources/etcd-pod-1/../etcd-pod-99"), 0600)
		}, diagnostic: "Invalid or unreadable revision reference"},
		{name: "noncanonical prefix", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, fixture.manifest("etcd"), backupFixtureManifest("/unrelated"+fixture.root+"/static-pod-resources/etcd-pod-1"), 0600)
		}, diagnostic: "Invalid or unreadable revision reference"},
		{name: "dangling optional", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			require.NoError(test, os.Remove(fixture.manifest("kube-apiserver")))
			require.NoError(test, os.Symlink("absent", fixture.manifest("kube-apiserver")))
		}, diagnostic: "Invalid or unreadable manifest"},
		{name: "invalid optional resources", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			require.NoError(test, os.Remove(filepath.Join(fixture.root, "static-pod-resources/kube-apiserver-pod-1/kube-apiserver-pod.yaml")))
		}, diagnostic: "Invalid or unreadable revision resources"},
		{name: "symlinked optional resources", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			revision := filepath.Join(fixture.root, "static-pod-resources/kube-apiserver-pod-1")
			require.NoError(test, os.Rename(revision, revision+"-original"))
			require.NoError(test, os.Symlink(revision+"-original", revision))
		}, diagnostic: "Invalid or unreadable revision resources"},
		{name: "unreadable optional", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			fixture.env = append(fixture.env, "OC_MANIFEST_FAILURE=true")
		}, diagnostic: "Invalid or unreadable Pod manifest"},
		{name: "snapshot", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			fixture.env = append(fixture.env, "SNAPSHOT_EXIT=2")
		}},
		{name: "empty snapshot", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			fixture.env = append(fixture.env, "EMPTY_SNAPSHOT=true")
		}},
		{name: "integrity", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			fixture.env = append(fixture.env, "STATUS_EXIT=2")
		}, diagnostic: "Backup integrity verification failed"},
		{name: "tar", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, filepath.Join(fixture.root, "bin/tar"), "#!/bin/bash\necho broken > \"$2\"\nexit 2\n", 0700)
		}},
		{name: "empty tar", partial: true, configure: func(test *testing.T, fixture *clusterBackupFixture) {
			backupFixtureWrite(test, filepath.Join(fixture.root, "bin/tar"), "#!/bin/bash\ntouch \"$2\"\n", 0700)
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			scenario.configure(test, fixture)
			args := []string{"--force", fixture.out}
			if scenario.partial {
				args = append([]string{"--available-static-resources"}, args...)
			}
			output, err := fixture.run(test, args...)
			require.Error(test, err, output)
			require.Contains(test, output, scenario.diagnostic)
			require.NotContains(test, output, "saved to")
			fixture.requireNoArtifacts(test)
		})
	}
}

func TestClusterBackupRolloutChecks(test *testing.T) {
	test.Parallel()
	for _, partial := range []bool{false, true} {
		for _, status := range []string{"True", "", "False", "missing kubeconfig", "API error"} {
			test.Run(fmt.Sprintf("partial=%t/%s", partial, status), func(test *testing.T) {
				fixture := newClusterBackupFixture(test)
				fixture.env = append(fixture.env, "PROGRESSING="+status)
				if status == "missing kubeconfig" {
					require.NoError(test, os.Remove(fixture.kubeconfig()))
				} else if status == "API error" {
					fixture.env = append(fixture.env, "OC_FAILURE=true")
				}
				args := []string{fixture.out}
				if partial {
					fixture.removeOptional(test)
					args = append([]string{"--available-static-resources"}, args...)
				}
				output, err := fixture.run(test, args...)
				if status == "False" {
					require.NoError(test, err, output)
				} else {
					require.Error(test, err, output)
					fixture.requireNoArtifacts(test)
				}
				if status == "False" {
					return
				}
				output, err = fixture.run(test, append([]string{"--force"}, args...)...)
				require.NoError(test, err, output)
			})
		}
	}
}

func TestClusterBackupArguments(test *testing.T) {
	test.Parallel()
	for _, args := range [][]string{nil, {"--force"}, {"--available-static-resources"}, {"--unknown"}, {"one", "two"}, {"--"}, {"--", "one", "two"}} {
		test.Run(fmt.Sprint(args), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			output, err := fixture.run(test, args...)
			require.Error(test, err, output)
			require.Contains(test, output, "Usage:")
			fixture.requireNoArtifacts(test)
		})
	}
	for _, args := range [][]string{{"--force", "--available-static-resources"}, {"--available-static-resources", "--force"}, {"--available-static-resources", "--force", "--force"}, {"--force", "--"}} {
		test.Run(fmt.Sprint(args), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			output, err := fixture.run(test, append(args, fixture.out)...)
			require.NoError(test, err, output)
		})
	}
}

func TestClusterBackupPartialChecksEveryOperator(test *testing.T) {
	test.Parallel()
	for _, progressing := range []string{"", "kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		test.Run("progressing="+progressing, func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			fixture.removeOptional(test)
			fixture.env = append(fixture.env, "PROGRESSING_OPERATOR="+progressing)
			output, err := fixture.run(test, "--available-static-resources", fixture.out)
			if progressing == "" {
				require.NoError(test, err, output)
				calls, err := os.ReadFile(filepath.Join(fixture.root, "operator-calls"))
				require.NoError(test, err)
				require.Equal(test, "etcd\nkube-apiserver\nkube-controller-manager\nkube-scheduler\n", string(calls))
			} else {
				require.Error(test, err, output)
				require.Contains(test, output, "Currently the "+progressing+" operator is progressing")
				fixture.requireNoArtifacts(test)
			}
		})
	}
}

func TestClusterBackupMissingOptionalRevision(test *testing.T) {
	test.Parallel()
	fixture := newClusterBackupFixture(test)
	require.NoError(test, os.RemoveAll(filepath.Join(fixture.root, "static-pod-resources/kube-apiserver-pod-1")))
	output, err := fixture.run(test, "--available-static-resources", "--force", fixture.out)
	require.NoError(test, err, output)
	require.Contains(test, output, "OMITTED kube-apiserver: referenced local revision is absent")
	archives, err := filepath.Glob(filepath.Join(fixture.out, "*.tar.gz"))
	require.NoError(test, err)
	require.Len(test, archives, 1)
	contents := backupArchiveContents(test, archives[0])
	require.Len(test, contents, 10)
	require.NotContains(test, contents, "static-pod-resources/kube-apiserver-pod-99/not-active")
}

func TestClusterBackupRootCheck(test *testing.T) {
	fixture := newClusterBackupFixture(test)
	fixture.env = append(fixture.env, "FIXTURE_EUID=1000")
	output, err := fixture.run(test, "--force", fixture.out)
	require.Error(test, err, output)
	require.Contains(test, output, "This script must be run as root")
	fixture.requireNoArtifacts(test)
}

func TestClusterBackupRelativeOutput(test *testing.T) {
	test.Parallel()
	fixture := newClusterBackupFixture(test)
	fixture.out = filepath.Join(fixture.root, "relative backup")
	output, err := fixture.run(test, "--available-static-resources", "--force", "relative backup")
	require.NoError(test, err, output)
	archives, err := filepath.Glob(filepath.Join(fixture.out, "partial_*.tar.gz"))
	require.NoError(test, err)
	require.Len(test, archives, 1)
	require.Contains(test, backupArchiveContents(test, archives[0]), "PARTIAL_BACKUP.txt")
}

func TestClusterBackupPreservesPreviousOutputs(test *testing.T) {
	test.Parallel()
	for _, partial := range []bool{false, true} {
		test.Run(fmt.Sprintf("partial=%t", partial), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			args := []string{"--force", fixture.out}
			if partial {
				args = append([]string{"--available-static-resources"}, args...)
			}
			output, err := fixture.run(test, args...)
			require.NoError(test, err, output)
			entries, err := os.ReadDir(fixture.out)
			require.NoError(test, err)
			require.Len(test, entries, 2)
			previous := map[string][]byte{}
			for _, entry := range entries {
				previous[entry.Name()], err = os.ReadFile(filepath.Join(fixture.out, entry.Name()))
				require.NoError(test, err)
			}
			if !partial {
				output, err = fixture.run(test, args...)
				require.Error(test, err, output)
				require.NotContains(test, output, "saved to")
			}
			fixture.env = append(fixture.env, "SNAPSHOT_EXIT=2")
			output, err = fixture.run(test, args...)
			require.Error(test, err, output)
			require.NotContains(test, output, "saved to")
			for name, content := range previous {
				current, err := os.ReadFile(filepath.Join(fixture.out, name))
				require.NoError(test, err)
				require.Equal(test, content, current)
			}
			entries, err = os.ReadDir(fixture.out)
			require.NoError(test, err)
			require.Len(test, entries, 2)
		})
	}
}

func TestClusterBackupOfflineOCParsing(test *testing.T) {
	test.Parallel()
	realOC, err := exec.LookPath("oc")
	if err != nil {
		test.Skip("optional offline smoke test requires oc; deterministic decoder fixtures still run")
	}
	for _, malformed := range []bool{false, true} {
		test.Run(fmt.Sprintf("malformed=%t", malformed), func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			fixture.removeOptional(test)
			fixture.env = append(fixture.env, "REAL_OC="+realOC)
			if malformed {
				backupFixtureWrite(test, fixture.manifest("etcd"), "# "+fixture.root+"/static-pod-resources/etcd-pod-1\n", 0600)
			}
			output, err := fixture.run(test, "--available-static-resources", "--force", fixture.out)
			if malformed {
				require.Error(test, err, output)
				fixture.requireNoArtifacts(test)
			} else {
				require.NoError(test, err, output)
			}
		})
	}
}

func TestClusterBackupPublicationFailure(test *testing.T) {
	test.Parallel()
	for _, kind := range []string{"archive", "snapshot", "directory"} {
		test.Run(kind, func(test *testing.T) {
			fixture := newClusterBackupFixture(test)
			archive := filepath.Join(fixture.out, "static_kuberesources_2026-01-01_000000__POSSIBLY_DIRTY__.tar.gz")
			snapshot := filepath.Join(fixture.out, "snapshot_2026-01-01_000000__POSSIBLY_DIRTY__.db")
			existing := archive
			if kind == "snapshot" {
				existing = snapshot
			}
			if kind == "directory" {
				require.NoError(test, os.MkdirAll(existing, 0700))
			} else {
				backupFixtureWrite(test, existing, "previous output", 0600)
			}
			output, err := fixture.run(test, "--force", fixture.out)
			require.Error(test, err, output)
			require.NotContains(test, output, "saved to")
			if kind == "directory" {
				entries, err := os.ReadDir(existing)
				require.NoError(test, err)
				require.Empty(test, entries)
			} else {
				content, err := os.ReadFile(existing)
				require.NoError(test, err)
				require.Equal(test, "previous output", string(content))
			}
			entries, err := os.ReadDir(fixture.out)
			require.NoError(test, err)
			require.Len(test, entries, 1)
		})
	}
}

func TestClusterBackupSyntax(test *testing.T) {
	command := exec.Command("bash", "-n")
	command.Stdin = strings.NewReader(string(bindata.MustAsset("etcd/cluster-backup.sh")))
	output, err := command.CombinedOutput()
	require.NoError(test, err, string(output))
}

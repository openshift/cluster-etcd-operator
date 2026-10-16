package scriptcontroller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/stretchr/testify/require"
)

func runQuorumRestoreShell(test *testing.T, script string, environment []string, arguments ...string) (string, int, string) {
	test.Helper()
	testDir := test.TempDir()
	for filename, asset := range map[string]string{
		"quorum-restore.sh": "etcd/quorum-restore.sh",
		"etcd-common-tools": "etcd/etcd-common-tools",
	} {
		require.NoError(test, os.WriteFile(filepath.Join(testDir, filename), bindata.MustAsset(asset), 0600))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", append([]string{"-c", `
source "$1/quorum-restore.sh"
source "$1/etcd-common-tools"
TEST_DIR="$1"
export TEST_DIR
shift
` + script, "quorum-restore-test", testDir}, arguments...)...)
	command.WaitDelay = time.Second
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	require.NoError(test, ctx.Err(), "shell test exceeded its deadline: %s", output)
	status := 0
	if err != nil {
		var exitError *exec.ExitError
		require.ErrorAs(test, err, &exitError, "%s", output)
		status = exitError.ExitCode()
	}
	return string(output), status, testDir
}

func TestQuorumRestoreArguments(test *testing.T) {
	test.Parallel()
	testCases := []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "no wait", want: "false:300"},
		{name: "default wait", arguments: []string{"--wait"}, want: "true:300"},
		{name: "custom wait", arguments: []string{"--wait", "600"}, want: "true:600"},
		{name: "decimal zero prefix", arguments: []string{"--wait", "000008"}, want: "true:8"},
		{name: "maximum", arguments: []string{"--wait", "2147483647"}, want: "true:2147483647"},
		{name: "repeated bare flags", arguments: []string{"--wait", "--wait"}, want: "true:300"},
		{name: "bare flag preserves limit", arguments: []string{"--wait", "600", "--wait"}, want: "true:600"},
		{name: "last numeric limit wins", arguments: []string{"--wait", "600", "--wait", "10"}, want: "true:10"},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			output, status, _ := runQuorumRestoreShell(test, `parse_arguments "$@"; printf '%s:%s' "$WAIT_FOR_ETCD" "$WAIT_TIMEOUT"`, nil, testCase.arguments...)
			require.Equal(test, 0, status, output)
			require.Equal(test, testCase.want, output)
		})
	}
	for _, arguments := range [][]string{
		{"--timeout", "600"}, {"--help"}, {"--wait=600"}, {"600"}, {"--wait", "1", "2"},
		{"--wait", "0"}, {"--wait", "000"}, {"--wait", "-1"}, {"--wait", "+1"},
		{"--wait", "1.5"}, {"--wait", "1s"}, {"--wait", ""}, {"--wait", " 1"},
		{"--wait", "2147483648"}, {"--wait", strings.Repeat("9", 100)},
	} {
		test.Run(fmt.Sprintf("invalid %q", arguments), func(test *testing.T) {
			test.Parallel()
			output, status, _ := runQuorumRestoreShell(test, `
source_required_dependency() { echo 'unexpected dependency access'; exit 90; }
restore_quorum() { echo 'unexpected restore'; exit 91; }
main "$@"
`, nil, arguments...)
			require.Equal(test, 2, status, output)
			require.Contains(test, output, "Usage:")
			require.NotContains(test, output, "unexpected")
			require.NotContains(test, output, "root")
		})
	}
}

func TestQuorumRestoreWorkflow(test *testing.T) {
	test.Parallel()
	testCases := []struct {
		name        string
		arguments   []string
		environment []string
		wantStatus  int
		wantEvents  string
	}{
		{name: "no wait needs no new prerequisites", wantEvents: "move\nstop\ncopy\n"},
		{name: "default wait", arguments: []string{"--wait"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "custom wait", arguments: []string{"--wait", "600"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:600\n"},
		{name: "failed copy never waits", arguments: []string{"--wait"}, environment: []string{"COPY_STATUS=17"}, wantStatus: 17, wantEvents: "preflight\nmove\nstop\ncopy\n"},
		{name: "wait failure propagates", arguments: []string{"--wait"}, environment: []string{"WAIT_STATUS=1"}, wantStatus: 1, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "wait timeout propagates", arguments: []string{"--wait"}, environment: []string{"WAIT_STATUS=124"}, wantStatus: 124, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "missing crictl", arguments: []string{"--wait"}, environment: []string{"MISSING_TOOL=crictl"}, wantStatus: 1},
		{name: "missing timeout", arguments: []string{"--wait"}, environment: []string{"MISSING_TOOL=timeout"}, wantStatus: 1},
		{name: "incompatible timeout", arguments: []string{"--wait"}, environment: []string{"PREFLIGHT_STATUS=1"}, wantStatus: 1, wantEvents: "preflight\n"},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			output, status, testDir := runQuorumRestoreShell(test, `
parse_arguments "$@"
command() {
  [[ "$WAIT_FOR_ETCD" == true ]] || { echo 'unexpected preflight'; return 90; }
  [[ "$1" == -v && "$2" != "${MISSING_TOOL:-}" ]]
}
timeout() {
  [[ "$*" == '--signal=KILL 1s true' ]] || return 91
  echo preflight >> "$TEST_DIR/events"
  return "${PREFLIGHT_STATUS:-0}"
}
mv_static_pods() {
  [[ "$*" == etcd-pod.yaml ]] || return 92
  echo move >> "$TEST_DIR/events"
}
wait_for_containers_to_stop() {
  [[ "$*" == 'etcd etcdctl etcd-metrics etcd-readyz etcd-rev' ]] || return 93
  echo stop >> "$TEST_DIR/events"
}
cp() {
  [[ "$*" == "$QUORUM_RESTORE_ETCD_POD_YAML $MANIFEST_DIR/etcd-pod.yaml" ]] || return 94
  echo copy >> "$TEST_DIR/events"
  return "${COPY_STATUS:-0}"
}
wait_for_restored_etcd_to_start() {
  echo "wait:$1" >> "$TEST_DIR/events"
  return "${WAIT_STATUS:-0}"
}
restore_quorum
`, testCase.environment, testCase.arguments...)
			require.Equal(test, testCase.wantStatus, status, output)
			events, err := os.ReadFile(filepath.Join(testDir, "events"))
			if testCase.wantEvents == "" {
				require.ErrorIs(test, err, os.ErrNotExist)
			} else {
				require.NoError(test, err)
				require.Equal(test, testCase.wantEvents, string(events))
			}
			if len(testCase.arguments) == 0 {
				require.Equal(test, "starting restore-etcd static pod\n", output)
			}
		})
	}
}

func TestWaitForRestoredEtcd(test *testing.T) {
	test.Parallel()
	testCases := []struct {
		name            string
		environment     []string
		wantStatus      int
		wantCalls       int
		requiresTimeout bool
	}{
		{name: "immediate RUNNING", wantCalls: 1},
		{name: "delayed RUNNING", environment: []string{"RUNNING_AFTER=3"}, wantCalls: 3},
		{name: "nonrunning and unrelated containers", environment: []string{"CONTAINERS=etcd openshift-etcd Created created\netcd openshift-etcd Exited exited\netcd elsewhere Running other\netcd-readyz openshift-etcd Running sidecar"}, wantStatus: 124, wantCalls: 3},
		{name: "empty results time out", environment: []string{"CONTAINERS="}, wantStatus: 124, wantCalls: 3},
		{name: "crictl error is not absence", environment: []string{"CRI_STATUS=7"}, wantStatus: 1, wantCalls: 1},
		{name: "crictl error with output is not success", environment: []string{"CRI_STATUS=7", "ERROR_OUTPUT=container-id"}, wantStatus: 1, wantCalls: 1},
		{name: "timeout utility failure", environment: []string{"TIMEOUT_STATUS=125"}, wantStatus: 1},
		{name: "late RUNNING rejected", environment: []string{"WAIT_LIMIT=1", "LOOKUP_SLEEP=1.2"}, wantStatus: 124, wantCalls: 1},
		{name: "hung CRI bounded by real timeout", environment: []string{"WAIT_LIMIT=1", "LOOKUP_SLEEP=30", "REAL_TIMEOUT=true"}, wantStatus: 124, wantCalls: 1, requiresTimeout: true},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if testCase.requiresTimeout {
				version, err := exec.CommandContext(test.Context(), "timeout", "--version").CombinedOutput()
				if err != nil || !strings.Contains(string(version), "GNU coreutils") {
					test.Skip("native timeout integration requires GNU coreutils; mocked cases remain active")
				}
			}
			output, status, testDir := runQuorumRestoreShell(test, `
cat > "$TEST_DIR/crictl" <<'EOF'
#!/usr/bin/env bash
set -e
count=0
[[ ! -f "$TEST_DIR/count" ]] || read -r count < "$TEST_DIR/count"
count=$((count + 1))
echo "$count" > "$TEST_DIR/count"
echo "$*" >> "$TEST_DIR/calls"
[[ -z "${LOOKUP_SLEEP:-}" ]] || /bin/sleep "$LOOKUP_SLEEP"
if [[ "${CRI_STATUS:-0}" != 0 ]]; then
  printf '%s' "${ERROR_OUTPUT:-}"
  exit "$CRI_STATUS"
fi
(( count >= ${RUNNING_AFTER:-1} )) || exit 0
while read -r name namespace state identifier; do
  if [[ "$name" == etcd && "$namespace" == openshift-etcd && "$state" == Running ]]; then
    echo "$identifier"
  fi
done <<< "${CONTAINERS-etcd openshift-etcd Running restored}"
EOF
chmod +x "$TEST_DIR/crictl"
PATH="$TEST_DIR:$PATH"
export PATH
timeout() {
  if [[ "${REAL_TIMEOUT:-false}" == true ]]; then
    command timeout "$@"
    return
  fi
  echo "$2" >> "$TEST_DIR/budgets"
  [[ "${TIMEOUT_STATUS:-0}" == 0 ]] || return "$TIMEOUT_STATUS"
  [[ "$1" == --signal=KILL ]] || return 95
  shift 2
  "$@"
}
sleep() { SECONDS=$((SECONDS + 1)); }
if [[ -z "${LOOKUP_SLEEP:-}" ]]; then
  unset SECONDS
fi
SECONDS=0
wait_for_restored_etcd_to_start "${WAIT_LIMIT:-3}"
`, testCase.environment)
			require.Equal(test, testCase.wantStatus, status, output)
			calls, err := os.ReadFile(filepath.Join(testDir, "calls"))
			if testCase.wantCalls == 0 {
				require.ErrorIs(test, err, os.ErrNotExist)
			} else {
				require.NoError(test, err)
				require.Equal(test, strings.Repeat("ps --state Running --label io.kubernetes.container.name=etcd --label io.kubernetes.pod.namespace=openshift-etcd -q\n", testCase.wantCalls), string(calls))
			}
			switch testCase.wantStatus {
			case 0:
				require.Contains(test, output, "Restored etcd container is RUNNING")
			case 1:
				require.Contains(test, output, "Unable to determine restored etcd container state")
			case 124:
				require.Contains(test, output, "Timed out after")
				require.Contains(test, output, "restore manifest remains installed")
				require.NotContains(test, output, "Restored etcd container is RUNNING")
			}
			if testCase.wantCalls == 3 {
				budgets, err := os.ReadFile(filepath.Join(testDir, "budgets"))
				require.NoError(test, err)
				require.Equal(test, "3s\n2s\n1s\n", string(budgets))
			}
		})
	}
}

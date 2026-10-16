package scriptcontroller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/stretchr/testify/assert"
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
		wantError   string
	}{
		{name: "no wait needs no new prerequisites", wantEvents: "move\nstop\ncopy\n"},
		{name: "default wait", arguments: []string{"--wait"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "custom wait", arguments: []string{"--wait", "600"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:600\n"},
		{name: "failed copy never waits", arguments: []string{"--wait"}, environment: []string{"COPY_STATUS=17"}, wantStatus: 17, wantEvents: "preflight\nmove\nstop\ncopy\n"},
		{name: "wait failure propagates", arguments: []string{"--wait"}, environment: []string{"WAIT_STATUS=1"}, wantStatus: 1, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "wait timeout propagates", arguments: []string{"--wait"}, environment: []string{"WAIT_STATUS=124"}, wantStatus: 124, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "missing curl", arguments: []string{"--wait"}, environment: []string{"MISSING_TOOL=curl"}, wantStatus: 1, wantError: "curl is required"},
		{name: "missing timeout", arguments: []string{"--wait"}, environment: []string{"MISSING_TOOL=timeout"}, wantStatus: 1, wantError: "GNU coreutils timeout"},
		{name: "incompatible timeout", arguments: []string{"--wait"}, environment: []string{"PREFLIGHT_STATUS=1"}, wantStatus: 1, wantEvents: "preflight\n"},
		{name: "configured CA with spaces", arguments: []string{"--wait"}, environment: []string{"CA_MODE=custom"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "valid configured CA wins", arguments: []string{"--wait"}, environment: []string{"CA_MODE=source"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "host mapped CA", arguments: []string{"--wait"}, environment: []string{"CA_MODE=mapped"}, wantEvents: "preflight\nmove\nstop\ncopy\nwait:300\n"},
		{name: "missing CA", arguments: []string{"--wait"}, environment: []string{"CA_MODE=missing"}, wantStatus: 1, wantError: "readable ETCDCTL_CACERT"},
		{name: "missing mapped CA", arguments: []string{"--wait"}, environment: []string{"CA_MODE=missing-mapped"}, wantStatus: 1, wantError: "readable ETCDCTL_CACERT"},
		{name: "unreadable configured CA", arguments: []string{"--wait"}, environment: []string{"CA_MODE=unreadable"}, wantStatus: 1, wantError: "readable ETCDCTL_CACERT"},
	}
	for _, testCase := range testCases {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			output, status, testDir := runQuorumRestoreShell(test, `
parse_arguments "$@"
ETCDCTL_CACERT="$TEST_DIR/ca.pem"
: > "$ETCDCTL_CACERT"
case "${CA_MODE:-}" in
  source|mapped|missing-mapped)
    ETCDCTL_CACERT=/etc/kubernetes/static-pod-certs/configmaps/etcd-all-bundles/server-ca-bundle.crt
    if [[ "$CA_MODE" == mapped ]]; then
      EXPECTED_CA=/etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-all-bundles/server-ca-bundle.crt
    fi
    ;;
  custom) ETCDCTL_CACERT="$TEST_DIR/custom ca.pem"; : > "$ETCDCTL_CACERT" ;;
  missing) unset ETCDCTL_CACERT ;;
  unreadable) ETCDCTL_CACERT=/configured/unreadable-ca.pem ;;
esac
test() {
  [[ "$WAIT_FOR_ETCD" == true ]] || { echo 'unexpected CA preflight'; return 90; }
  case "$2" in
    /etc/kubernetes/static-pod-certs/*) [[ "$CA_MODE" == source ]] || return 1 ;;
    /etc/kubernetes/static-pod-resources/etcd-certs/*) [[ "$CA_MODE" == mapped ]] || return 1 ;;
    /configured/unreadable-ca.pem) [[ "$1" == -f ]] || return 1 ;;
    *) builtin test "$@" || return 1 ;;
  esac
  if [[ "$1" == -r ]]; then
    : > "$TEST_DIR/ca-checked"
  fi
}
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
  if "$WAIT_FOR_ETCD"; then
    [[ -f "$TEST_DIR/ca-checked" ]] || return 98
  fi
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
  [[ "$2" == "${EXPECTED_CA:-$ETCDCTL_CACERT}" ]] || return 96
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
			if testCase.wantError != "" {
				require.Contains(test, output, testCase.wantError)
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
		{name: "immediate HTTP 200", wantCalls: 1},
		{name: "503 then 200", environment: []string{"READY_AFTER=3"}, wantCalls: 3},
		{name: "connection refused then 200", environment: []string{"READY_AFTER=3", "INITIAL_CURL_STATUS=7", "INITIAL_HTTP_STATUS=000"}, wantCalls: 3},
		{name: "503 never succeeds", environment: []string{"HTTP_STATUS=503"}, wantStatus: 124, wantCalls: 3},
		{name: "204 never succeeds", environment: []string{"HTTP_STATUS=204"}, wantStatus: 124, wantCalls: 3},
		{name: "redirect never succeeds", environment: []string{"HTTP_STATUS=302"}, wantStatus: 124, wantCalls: 3},
		{name: "404 never succeeds", environment: []string{"HTTP_STATUS=404"}, wantStatus: 124, wantCalls: 3},
		{name: "connection failure with 200 output", environment: []string{"CURL_STATUS=7"}, wantStatus: 124, wantCalls: 3},
		{name: "TLS verification failure", environment: []string{"CURL_STATUS=60"}, wantStatus: 1, wantCalls: 1},
		{name: "unusable CA", environment: []string{"CURL_STATUS=77"}, wantStatus: 1, wantCalls: 1},
		{name: "curl error", environment: []string{"CURL_STATUS=2"}, wantStatus: 1, wantCalls: 1},
		{name: "timeout utility failure", environment: []string{"TIMEOUT_STATUS=125"}, wantStatus: 1},
		{name: "deadline prevents request", environment: []string{"WAIT_LIMIT=0"}, wantStatus: 124},
		{name: "late 200 rejected", environment: []string{"WAIT_LIMIT=1", "LOOKUP_SLEEP=1.2"}, wantStatus: 124, wantCalls: 1},
		{name: "hung request bounded by real timeout", environment: []string{"WAIT_LIMIT=1", "LOOKUP_SLEEP=30", "REAL_TIMEOUT=true"}, wantStatus: 124, wantCalls: 1, requiresTimeout: true},
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
cat > "$TEST_DIR/curl" <<'EOF'
#!/usr/bin/env bash
set -e
[[ "$#" == 12 ]] || exit 95
count=0
[[ ! -f "$TEST_DIR/count" ]] || read -r count < "$TEST_DIR/count"
count=$((count + 1))
echo "$count" > "$TEST_DIR/count"
echo "$*" >> "$TEST_DIR/calls"
[[ -z "${LOOKUP_SLEEP:-}" ]] || /bin/sleep "$LOOKUP_SLEEP"
if (( count < ${READY_AFTER:-1} )); then
  printf '%s' "${INITIAL_HTTP_STATUS:-503}"
  exit "${INITIAL_CURL_STATUS:-0}"
fi
printf '%s' "${HTTP_STATUS-200}"
exit "${CURL_STATUS:-0}"
EOF
cat > "$TEST_DIR/crictl" <<'EOF'
#!/usr/bin/env bash
echo unexpected-crictl >> "$TEST_DIR/crictl-calls"
exit 99
EOF
chmod +x "$TEST_DIR/curl" "$TEST_DIR/crictl"
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
sleep() { echo sleep >> "$TEST_DIR/sleeps"; SECONDS=$((SECONDS + 1)); }
if [[ -z "${LOOKUP_SLEEP:-}" ]]; then
  unset SECONDS
fi
SECONDS=0
wait_for_restored_etcd_to_start "${WAIT_LIMIT:-3}" "$TEST_DIR/test ca.pem"
`, testCase.environment)
			require.Equal(test, testCase.wantStatus, status, output)
			calls, err := os.ReadFile(filepath.Join(testDir, "calls"))
			if testCase.wantCalls == 0 {
				require.ErrorIs(test, err, os.ErrNotExist)
			} else {
				require.NoError(test, err)
				require.Equal(test, strings.Repeat("--disable --silent --show-error --noproxy * --cacert "+testDir+"/test ca.pem --output /dev/null --write-out %{http_code} https://localhost:9980/readyz\n", testCase.wantCalls), string(calls))
			}
			_, err = os.Stat(filepath.Join(testDir, "crictl-calls"))
			require.ErrorIs(test, err, os.ErrNotExist)
			switch testCase.wantStatus {
			case 0:
				require.Contains(test, output, "Restored etcd readyz reports local readiness (HTTP 200)")
				if testCase.wantCalls == 1 {
					_, err := os.Stat(filepath.Join(testDir, "sleeps"))
					require.ErrorIs(test, err, os.ErrNotExist)
				}
			case 1:
				require.Contains(test, output, "Unable to check restored etcd readiness")
				require.Contains(test, output, "restore manifest remains installed")
			case 124:
				require.Contains(test, output, "Timed out after")
				require.Contains(test, output, "restore manifest remains installed")
				require.NotContains(test, output, "Restored etcd readyz reports local readiness")
			}
			if testCase.wantCalls == 3 {
				budgets, err := os.ReadFile(filepath.Join(testDir, "budgets"))
				require.NoError(test, err)
				require.Equal(test, "3s\n2s\n1s\n", string(budgets))
			}
		})
	}
}

func newReadyzTestCertificate(test *testing.T) (tls.Certificate, []byte) {
	test.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(test, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(test, err)
	return tls.Certificate{Certificate: [][]byte{certificate}, PrivateKey: privateKey}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
}

func TestWaitForRestoredEtcdHTTPS(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name         string
		untrustedCA  bool
		readyAfter   int32
		hung         bool
		wantStatus   int
		wantRequests int32
	}{
		{name: "empty 200 succeeds immediately", readyAfter: 1, wantRequests: 1},
		{name: "503 then empty 200", readyAfter: 2, wantRequests: 2},
		{name: "untrusted CA fails despite curlrc", untrustedCA: true, wantStatus: 1},
		{name: "hung HTTPS request obeys short deadline", hung: true, wantStatus: 124, wantRequests: 1},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			realCurl, err := exec.LookPath("curl")
			if err != nil {
				test.Skip("native HTTPS integration requires curl; mocked cases remain active")
			}
			version, err := exec.CommandContext(test.Context(), "timeout", "--version").CombinedOutput()
			if err != nil || !strings.Contains(string(version), "GNU coreutils") {
				test.Skip("native HTTPS integration requires GNU timeout; mocked cases remain active")
			}
			certificate, ca := newReadyzTestCertificate(test)
			var requests atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				assert.Equal(test, "/readyz", request.URL.Path)
				assert.Empty(test, request.TLS.PeerCertificates)
				count := requests.Add(1)
				if testCase.hung {
					<-request.Context().Done()
					return
				}
				if count < testCase.readyAfter {
					response.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				response.WriteHeader(http.StatusOK)
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
			server.StartTLS()
			test.Cleanup(server.Close)
			if testCase.untrustedCA {
				_, ca = newReadyzTestCertificate(test)
			}
			caPath := filepath.Join(test.TempDir(), "test ca.pem")
			require.NoError(test, os.WriteFile(caPath, ca, 0600))
			limit := "3"
			if testCase.hung {
				limit = "1"
			}
			output, status, _ := runQuorumRestoreShell(test, `
printf '%s\n' insecure > "$TEST_DIR/.curlrc"
export CURL_HOME="$TEST_DIR"
cat > "$TEST_DIR/curl" <<'EOF'
#!/usr/bin/env bash
arguments=()
for argument in "$@"; do
  if [[ "$argument" == https://localhost:9980/readyz ]]; then
    arguments+=("$READYZ_TEST_URL")
  else
    arguments+=("$argument")
  fi
done
exec "$TEST_REAL_CURL" "${arguments[@]}"
EOF
chmod +x "$TEST_DIR/curl"
export PATH="$TEST_DIR:$PATH"
wait_for_restored_etcd_to_start "$WAIT_LIMIT" "$TEST_CA"
`, []string{
				"TEST_REAL_CURL=" + realCurl,
				"READYZ_TEST_URL=" + server.URL + "/readyz", "TEST_CA=" + caPath, "WAIT_LIMIT=" + limit,
				"HTTPS_PROXY=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1",
				"ALL_PROXY=http://127.0.0.1:1", "all_proxy=http://127.0.0.1:1", "NO_PROXY=", "no_proxy=",
			})
			require.Equal(test, testCase.wantStatus, status, output)
			require.Equal(test, testCase.wantRequests, requests.Load())
			if status == 0 {
				require.Contains(test, output, "readyz reports local readiness (HTTP 200)")
			} else {
				require.NotContains(test, output, "readyz reports local readiness (HTTP 200)")
				require.Contains(test, output, "restore manifest remains installed")
			}
		})
	}
}

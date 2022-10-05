package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	"go.etcd.io/etcd/tests/v3/integration"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/monitor/health"
	u "github.com/openshift/cluster-etcd-operator/pkg/testutils"
)

var (
	testTLSInfo = transport.TLSInfo{
		KeyFile:        u.MustAbsPath("../../testutils/testdata/server.key.insecure"),
		CertFile:       u.MustAbsPath("../../testutils/testdata/server.crt"),
		TrustedCAFile:  u.MustAbsPath("../../testutils/testdata/ca.crt"),
		ClientCertAuth: true,
	}
)

type testData struct {
	logDir     string
	logFile    *os.File
	testServer *integration.ClusterV3
	URL        *url.URL

	// duration is the duration that the test will run before the context expires default 10s
	duration                 time.Duration
	pauseServer              bool
	stopServer               bool
	stopServerAfterDuration  time.Duration
	pauseServerAfterDuration time.Duration
	pauseEtcdPeers           int
	stopEtcdPeers            int
	monitorOpts              monitorOpts
}

func newTestData(t *testing.T, testDuration time.Duration, pauseServer, resumeServer, stopServer bool, pauseServerAfterDuration, resumeServerAfterDuration, stopServerAfterDuration time.Duration, pauseEtcdPeers, stopEtcdPeers int) *testData {
	logDir, err := ioutil.TempDir("/tmp", "health-check-test-")
	require.NoError(t, err)

	logFile, err := ioutil.TempFile(logDir, "health.*.log")
	require.NoError(t, err)

	testServer, targets := createAndStartEtcdTestServer(t, 3)

	td := &testData{
		logDir:                   logDir,
		logFile:                  logFile,
		duration:                 testDuration,
		testServer:               testServer,
		pauseServer:              pauseServer,
		pauseServerAfterDuration: pauseServerAfterDuration,
		pauseEtcdPeers:           pauseEtcdPeers,
		stopServer:               stopServer,
		stopServerAfterDuration:  stopServerAfterDuration,
		stopEtcdPeers:            stopEtcdPeers,
		monitorOpts: monitorOpts{
			Targets:          targets,
			logLevel:         4,
			interval:         1 * time.Second,
			logOutputs:       []string{"stderr", logFile.Name()},
			clientCertFile:   testTLSInfo.CertFile,
			clientKeyFile:    testTLSInfo.KeyFile,
			clientCACertFile: testTLSInfo.TrustedCAFile,
			podName:          "etcd-test",
		},
	}

	return td
}

func createAndStartEtcdTestServer(t *testing.T, size int) (*integration.ClusterV3, string) {
	srvTLS := testTLSInfo
	integration.BeforeTestExternal(t)
	etcd := integration.NewClusterV3(t, &integration.ClusterConfig{Size: size, ClientTLS: &srvTLS})
	targets := fmt.Sprintf("%s,%s,%s", etcd.Members[0].GRPCURL(), etcd.Members[1].GRPCURL(), etcd.Members[2].GRPCURL())

	// populated expected default NS
	etcd.Client(0).Put(context.Background(), health.DefaultNamespaceKey, "foo")
	return etcd, targets
}

func TestMonitor(t *testing.T) {
	testCases := map[string]struct {
		duration time.Duration
		// pauseServer if set to true the server process will pause during the test to introduce disruption.
		pauseServer bool
		// pauseServerAfterDuration is the duration the test will run before the server is paused. NOTE: pause will not
		// NOTE: Pause only affects linearized requests for non linearized requests use stop.
		pauseServerAfterDuration time.Duration
		// pauseEtcdPeers is the number of peers to be paused.
		pauseEtcdPeers          int
		stopServer              bool
		stopServerAfterDuration time.Duration
		stopEtcdPeers           int

		// resumeServer if set to true will resume a paused server.
		resumeServer bool
		// resumeServerAfterDuration is the duration the test will run before the server is resumed.
		resumeServerAfterDuration time.Duration
		// verify test output for a specific health check
		wantHealthCheck health.CheckName
		// the expected time duration of disruption.
		wantDuration time.Duration

		wantErr bool
	}{
		"healthy GRPCReadySingleTarget": {
			duration:        3 * time.Second,
			wantHealthCheck: health.GRPCReadySingleTarget,
			wantDuration:    0 * time.Second,
		},
		"healthy QuorumRead": {
			duration:        2 * time.Second,
			wantHealthCheck: health.QuorumRead,
			wantDuration:    0 * time.Second,
		},
		"unhealthy QuorumRead": {
			duration:                 5 * time.Second,
			pauseServer:              true,
			pauseServerAfterDuration: 0 * time.Second, //instantly
			pauseEtcdPeers:           2,
			wantHealthCheck:          health.QuorumRead,
			wantDuration:             5 * time.Second,
		},
		"unhealthy QuorumReadSingleTarget": {
			duration:                 6 * time.Second,
			pauseServer:              true,
			pauseServerAfterDuration: 0 * time.Second, //instantly
			pauseEtcdPeers:           2,
			wantHealthCheck:          health.QuorumReadSingleTarget,
			wantDuration:             6 * time.Second,
		},
		"healthy SerializedReadSingleTarget 7s 3s disruption": {
			duration:                10 * time.Second,
			stopServer:              true,
			stopEtcdPeers:           2,
			stopServerAfterDuration: 7 * time.Second,
			wantHealthCheck:         health.SerializedReadSingleTarget,
			wantDuration:            3 * time.Second,
		},
		"healthy GRPCReadySingleTarget 7s 3s disruption": {
			duration:                10 * time.Second,
			stopServer:              true,
			stopEtcdPeers:           2,
			stopServerAfterDuration: 7 * time.Second,
			wantHealthCheck:         health.GRPCReadySingleTarget,
			wantDuration:            3 * time.Second,
		},
		"monitor error from failed etcd client creation": {
			duration:                1 * time.Second,
			stopServer:              true,
			stopEtcdPeers:           2,
			stopServerAfterDuration: 1 * time.Millisecond,
			wantErr:                 true,
		},
	}
	for testName, tc := range testCases {
		t.Run(testName, func(t *testing.T) {
			// TODO cleanup
			td := newTestData(t,
				tc.duration,
				tc.pauseServer,
				tc.resumeServer,
				tc.stopServer,
				tc.pauseServerAfterDuration,
				tc.resumeServerAfterDuration,
				tc.stopServerAfterDuration,
				tc.pauseEtcdPeers,
				tc.stopEtcdPeers,
			)
			ctx, cancel := context.WithTimeout(context.Background(), td.duration)
			defer cancel()
			testServer := td.testServer

			if td.pauseServer {
				time.AfterFunc(td.pauseServerAfterDuration, func() {
					pauseEtcdPeers(testServer, td.pauseEtcdPeers)
				})
			}
			if td.stopServer {
				time.AfterFunc(td.stopServerAfterDuration, func() {
					stopEtcdPeers(t, testServer, td.stopEtcdPeers)
				})
			}
			err := td.monitorOpts.Run(ctx)
			if err != nil && !tc.wantErr {
				t.Fatalf("healthCheck unexpected error: %v", err)
			}
			_ = td.logFile.Close()
			// don't terminate if already stopped
			if !tc.stopServer {
				td.testServer.Terminate(t)
			}
			if err != nil && tc.wantErr {
				// error expected nothing else to do
				return
			}
			// check logs for disruption
			gotDuration, err := getDisruptionDurationFromLogs(t, tc.wantHealthCheck, td.logFile.Name(), td.logDir)
			require.NoError(t, err)

			assert.InDeltaf(t, tc.wantDuration, gotDuration, float64(1*time.Second),
				"healthCheck %v want: %v got %v", tc.wantHealthCheck, tc.wantDuration, gotDuration)
		})
	}
}

func pauseEtcdPeers(testServer *integration.ClusterV3, peers int) {
	i := 0
	for i <= peers {
		testServer.Members[i].Pause()
		i++
	}
}

func stopEtcdPeers(t *testing.T, testServer *integration.ClusterV3, peers int) {
	i := 0
	for i <= peers {
		testServer.Members[i].Stop(t)
		i++
	}
}
func cleanup(logDir string) {
	if err := os.RemoveAll(logDir); err != nil {
		panic(err)
	}
}

func getDisruptionDurationFromLogs(t *testing.T, wantHealthCheck health.CheckName, logPath, logDir string) (time.Duration, error) {
	var dur time.Duration // 0s
	logFile, err := os.Open(logPath)
	if err != nil {
		return dur, err
	}
	defer logFile.Close()

	decoder := json.NewDecoder(logFile)
	var log health.LogLine
	for decoder.More() {
		if err := decoder.Decode(&log); err != nil {
			return dur, fmt.Errorf("parse error: %w", err)
		}
		tsFormat := "2006-01-02T15:04:05.000Z" // RFC3339
		if log.DisruptionStart == "" || log.DisruptionEnd == "" || log.Check != string(wantHealthCheck) {
			continue
		}
		disruptionStart, err := time.Parse(tsFormat, log.DisruptionStart)
		require.NoError(t, err)
		disruptionEnd, err := time.Parse(tsFormat, log.DisruptionEnd)
		require.NoError(t, err)
		duration := disruptionEnd.Sub(disruptionStart)

		// round to nearest second
		if duration.Round(1*time.Second) > 0*time.Second {
			dur = duration.Round(1 * time.Second)
			break
		}
	}
	cleanup(logDir)
	return dur, nil
}

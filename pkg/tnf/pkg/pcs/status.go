package pcs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

const (
	// DefaultResourceWaitTimeout is used after stonith create/update (replaces --wait=120).
	DefaultResourceWaitTimeout = 2 * time.Minute
	// EtcdClusterIdleWait is the pcs status wait duration after etcd resource updates (replaces --wait=300).
	EtcdClusterIdleWait  = "5min"
	resourcePollInterval = 2 * time.Second
)

// WaitForClusterIdle waits until Pacemaker has no pending actions, up to timeout.
// timeout is a pcs duration string such as "2min" or "5min".
func WaitForClusterIdle(ctx context.Context, timeout string) error {
	cmd := fmt.Sprintf("/usr/sbin/pcs status wait %s", timeout)
	stdOut, stdErr, err := exec.Execute(ctx, cmd)
	if err != nil {
		return fmt.Errorf("pcs status wait failed: stdout=%s stderr=%s: %w", stdOut, stdErr, err)
	}
	return nil
}

// IsResourceStarted reports whether pcs considers the resource started.
// pcs status query prints "True"/"False" and exits non-zero when the predicate is false.
func IsResourceStarted(ctx context.Context, resourceID string) (bool, error) {
	cmd := fmt.Sprintf("/usr/sbin/pcs status query resource %s is-state started", resourceID)
	stdOut, stdErr, err := exec.Execute(ctx, cmd)
	out := strings.TrimSpace(stdOut)
	if err != nil {
		// Predicate false typically returns a non-zero exit with "False" on stdout.
		if strings.EqualFold(out, "False") {
			return false, nil
		}
		return false, fmt.Errorf("pcs status query resource %s failed: stdout=%s stderr=%s: %w", resourceID, stdOut, stdErr, err)
	}
	return strings.EqualFold(out, "True"), nil
}

// WaitForResourceStarted polls until the resource is started or timeout elapses.
// Unlike pcs status wait, this does not require the whole cluster to be idle, so
// unrelated pending actions (e.g. etcd start/stop) do not block stonith verification.
func WaitForResourceStarted(ctx context.Context, resourceID string, timeout time.Duration) error {
	klog.Infof("Waiting up to %s for resource %s to be started", timeout, resourceID)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		started, err := IsResourceStarted(ctx, resourceID)
		if err != nil {
			lastErr = err
		} else if started {
			klog.Infof("Resource %s is started", resourceID)
			return nil
		} else {
			lastErr = fmt.Errorf("resource %s is not started yet", resourceID)
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out waiting for resource %s to start: %w", resourceID, lastErr)
			}
			return fmt.Errorf("timed out waiting for resource %s to start", resourceID)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(resourcePollInterval):
		}
	}
}

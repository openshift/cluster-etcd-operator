package certwatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

const (
	// FallbackPollInterval is a safety-net poll in case fsnotify misses an
	// event (e.g. the directory is replaced atomically via symlink swap).
	FallbackPollInterval = 1 * time.Minute
	healthTimeout        = 5 * time.Minute
	healthPollInterval   = 5 * time.Second
	// debounceWindow coalesces rapid successive writes into a single restart.
	debounceWindow = 2 * time.Second
)

// Run watches CA bundle files in certDir for changes and restarts etcd
// on the local node when they change. It uses fsnotify for instant
// detection with a periodic fallback. No Kubernetes API access is
// needed — it reads cert files from a hostPath mount and acts via nsenter.
func Run(ctx context.Context, certDir string) error {
	hostname, err := getHostname(ctx)
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}
	klog.Infof("Starting cert watcher on node %s, watching %s", hostname, certDir)

	baseline, err := hashDir(certDir)
	if err != nil {
		return fmt.Errorf("failed to hash initial cert directory: %w", err)
	}
	if baseline == "" {
		return fmt.Errorf("cert directory %s is empty, refusing to start with empty baseline", certDir)
	}
	klog.Infof("Recorded baseline cert hash: %s", baseline)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(certDir); err != nil {
		return fmt.Errorf("failed to watch %s: %w", certDir, err)
	}

	fallback := time.NewTicker(FallbackPollInterval)
	defer fallback.Stop()

	var debounce *time.Timer
	debounceCh := make(<-chan time.Time)

	for {
		select {
		case <-ctx.Done():
			klog.Info("Cert watcher shutting down")
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("fsnotify watcher closed")
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				klog.V(4).Infof("fsnotify event: %s", event)
				if debounce == nil {
					debounce = time.NewTimer(debounceWindow)
					debounceCh = debounce.C
				} else {
					debounce.Reset(debounceWindow)
				}
			}

		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("fsnotify error channel closed")
			}
			klog.Warningf("fsnotify error: %v", watchErr)

		case <-debounceCh:
			debounce = nil
			debounceCh = make(<-chan time.Time)
			baseline, err = checkAndRestart(ctx, certDir, baseline, hostname)
			if err != nil {
				klog.Errorf("Failed to handle cert change: %v", err)
			}

		case <-fallback.C:
			baseline, err = checkAndRestart(ctx, certDir, baseline, hostname)
			if err != nil {
				klog.Errorf("Failed to handle cert change (fallback): %v", err)
			}
		}
	}
}

// checkAndRestart compares the current cert hash against baseline and
// restarts etcd if they differ. Each node restarts independently when it
// detects the change. The health check guards against restarting while
// the peer is already down, but does not guarantee strict serialization.
// restart_no_leave is set on the local node only to prevent the OCF agent
// from running force_new_cluster on the restart it is about to perform.
func checkAndRestart(ctx context.Context, certDir, baseline, hostname string) (string, error) {
	current, err := hashDir(certDir)
	if err != nil {
		return baseline, fmt.Errorf("failed to hash cert directory: %w", err)
	}
	if current == baseline || current == "" {
		return baseline, nil
	}

	if !isClusterHealthy(ctx) {
		klog.Infof("Cert change detected but etcd cluster not fully healthy, deferring restart to next poll")
		return baseline, nil
	}

	klog.Infof("CA bundle changed (old: %s, new: %s), restarting etcd on %s", baseline, current, hostname)

	if err := setRestartNoLeave(ctx, hostname); err != nil {
		return baseline, fmt.Errorf("failed to set restart_no_leave: %w", err)
	}
	defer clearRestartNoLeave(ctx, hostname)

	if err := restartEtcdContainer(ctx); err != nil {
		return baseline, fmt.Errorf("failed to restart etcd container: %w", err)
	}

	if err := waitForEtcdHealthy(ctx); err != nil {
		klog.Errorf("etcd did not become healthy after restart: %v", err)
		return baseline, nil
	}

	if hasPacemakerFailcount(ctx) {
		clearPacemakerFailcount(ctx)
	}

	klog.Infof("Updated baseline cert hash to: %s", current)
	return current, nil
}

func hashDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", err
		}
		h.Write([]byte(name))
		h.Write(data)
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func getHostname(ctx context.Context) (string, error) {
	stdout, _, err := exec.Execute(ctx, "hostname")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func setRestartNoLeave(ctx context.Context, hostname string) error {
	cmd := fmt.Sprintf(`crm_attribute --lifetime reboot --node "%s" --name restart_no_leave --update true`, hostname)
	if _, _, err := exec.Execute(ctx, cmd); err != nil {
		return fmt.Errorf("crm_attribute failed: %w", err)
	}
	klog.Infof("Set restart_no_leave on %s", hostname)
	return nil
}

func clearRestartNoLeave(ctx context.Context, hostname string) {
	cmd := fmt.Sprintf(`crm_attribute --lifetime reboot --node "%s" --name restart_no_leave --delete`, hostname)
	if _, _, err := exec.Execute(ctx, cmd); err != nil {
		klog.Warningf("Failed to clear restart_no_leave on %s: %v", hostname, err)
		return
	}
	klog.Infof("Cleared restart_no_leave on %s", hostname)
}

func restartEtcdContainer(ctx context.Context) error {
	if _, _, err := exec.Execute(ctx, "podman restart etcd"); err != nil {
		return fmt.Errorf("podman restart etcd failed: %w", err)
	}
	klog.Info("etcd container restarted via podman")
	return nil
}

func hasPacemakerFailcount(ctx context.Context) bool {
	stdout, _, err := exec.Execute(ctx, "pcs status xml")
	if err != nil {
		klog.Warningf("Failed to get pcs status xml: %v", err)
		return false
	}
	var status pacemaker.PacemakerResult
	if err := xml.Unmarshal([]byte(stdout), &status); err != nil {
		klog.Warningf("Failed to parse pcs status xml: %v", err)
		return false
	}
	for _, node := range status.NodeHistory.Node {
		for _, rh := range node.ResourceHistory {
			if rh.ID == "etcd" && rh.FailCount == "INFINITY" {
				return true
			}
		}
	}
	return false
}

func clearPacemakerFailcount(ctx context.Context) {
	klog.Info("Resetting etcd-clone failcount")
	if _, _, err := exec.Execute(ctx, "pcs resource failcount reset etcd-clone"); err != nil {
		klog.Warningf("Failed to reset etcd-clone failcount: %v", err)
	}
}

type endpointHealth struct {
	Endpoint string `json:"endpoint"`
	Health   bool   `json:"health"`
}

func isClusterHealthy(ctx context.Context) bool {
	stdout, _, err := exec.Execute(ctx, "podman exec etcd etcdctl endpoint health --cluster -w json")
	if err != nil {
		return false
	}
	var endpoints []endpointHealth
	if err := json.Unmarshal([]byte(stdout), &endpoints); err != nil {
		klog.Warningf("Failed to parse etcd health JSON: %v", err)
		return false
	}
	if len(endpoints) == 0 {
		return false
	}
	for _, ep := range endpoints {
		if !ep.Health {
			return false
		}
	}
	return true
}

func waitForEtcdHealthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for etcd health")
		case <-ticker.C:
			if isClusterHealthy(ctx) {
				klog.Info("etcd cluster healthy after restart")
				return nil
			}
			klog.V(4).Info("etcd health check not yet passing")
		}
	}
}

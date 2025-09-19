package disruptivevalidate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	wait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	poll             = 3 * time.Second
	timeoutSetup     = tools.SetupJobCompletedTimeout
	timeoutFencing   = tools.FencingJobCompletedTimeout
	timeoutAfter     = tools.SetupJobCompletedTimeout
	timeoutPeerJob   = 45 * time.Minute
	timeoutPCSUp     = 2 * time.Minute
	timeoutPCSDown   = 5 * time.Minute
	timeoutPCSBackUp = 10 * time.Minute
	timeoutEtcdOK    = 10 * time.Minute
	jobNamePrefix    = "tnf-disruptive-validate-job-"
)

func RunDisruptiveValidate() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-server.SetupSignalHandler(); cancel() }()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}

	// 1) Wait orchestrated jobs
	if err := waitForLabeledJob(ctx, kc, tools.JobTypeSetup.GetNameLabelValue(), 1, timeoutSetup); err != nil {
		return fmt.Errorf("setup not complete: %w", err)
	}
	if err := waitForLabeledJob(ctx, kc, tools.JobTypeFencing.GetNameLabelValue(), 1, timeoutFencing); err != nil {
		return fmt.Errorf("fencing not complete: %w", err)
	}
	if err := waitForLabeledJob(ctx, kc, tools.JobTypeAfterSetup.GetNameLabelValue(), 2, timeoutAfter); err != nil {
		return fmt.Errorf("after-setup not complete: %w", err)
	}

	// 2) Local / peer discovery
	clusterCfg, err := config.GetClusterConfig(ctx, kc)
	if err != nil {
		return err
	}
	local, peer, err := detectLocalAndPeer(ctx, kc, clusterCfg.NodeName1, clusterCfg.NodeName2)
	if err != nil {
		return err
	}
	klog.Infof("validate: local=%s peer=%s", local, peer)

	// 3) Lexicographic sequencing
	if err := waitPeerValidateIfSecond(ctx, kc, local, clusterCfg.NodeName1, clusterCfg.NodeName2); err != nil {
		return err
	}

	// Pre-fence health
	if err := waitEtcdTwoVoters(ctx, timeoutEtcdOK); err != nil {
		return fmt.Errorf("pre-fence etcd not healthy: %w", err)
	}

	// 4) PCS preflight
	if _, _, err := exec.Execute(ctx, `command -v pcs`); err != nil {
		return fmt.Errorf("pcs not found: %w", err)
	}
	if _, _, err := exec.Execute(ctx, `systemctl is-active pacemaker`); err != nil {
		return fmt.Errorf("pacemaker not active: %w", err)
	}
	if err := waitPCSState(ctx, boolp(true), peer, timeoutPCSUp); err != nil {
		return fmt.Errorf("peer %q not ONLINE pre-fence: %w", peer, err)
	}

	// 5) Fence → OFFLINE → ONLINE → etcd healthy
	out, _, ferr := exec.Execute(ctx, fmt.Sprintf(`/usr/sbin/pcs stonith fence %s`, peer))
	if ferr != nil {
		ls := out
		if i := strings.LastIndex(ls, "\n"); i >= 0 && i+1 < len(ls) {
			ls = ls[i+1:]
		}
		return fmt.Errorf("pcs fence %s failed: %w (last line: %s)", peer, ferr, strings.TrimSpace(ls))
	}
	if err := waitPCSState(ctx, boolp(false), peer, timeoutPCSDown); err != nil {
		return fmt.Errorf("peer didn't go OFFLINE: %w", err)
	}
	if err := waitPCSState(ctx, boolp(true), peer, timeoutPCSBackUp); err != nil {
		return fmt.Errorf("peer didn't return ONLINE: %w", err)
	}
	if err := waitEtcdTwoVoters(ctx, timeoutEtcdOK); err != nil {
		return fmt.Errorf("post-fence etcd not healthy: %w", err)
	}

	klog.Infof("validate: success local=%s peer=%s", local, peer)
	return nil
}

// helpers
func waitForLabeledJob(ctx context.Context, kc kubernetes.Interface, nameLabel string, wantAtLeast int, to time.Duration) error {
	sel := fmt.Sprintf("app.kubernetes.io/name=%s", nameLabel)
	return wait.PollUntilContextTimeout(ctx, poll, to, true, func(context.Context) (bool, error) {
		jl, err := kc.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil || len(jl.Items) < wantAtLeast {
			return false, nil
		}
		completed := 0
		for i := range jl.Items {
			j := &jl.Items[i]
			if j.Status.Succeeded > 0 || tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete) {
				completed++
			}
		}
		return completed >= wantAtLeast, nil
	})
}

func detectLocalAndPeer(_ context.Context, _ kubernetes.Interface, n1, n2 string) (string, string, error) {
	podName, err := os.Hostname()
	if err != nil || strings.TrimSpace(podName) == "" {
		return "", "", fmt.Errorf("get pod hostname: %w", err)
	}
	// "<job-name>-<suffix>" -> "<job-name>"
	i := strings.LastIndex(podName, "-")
	if i <= 0 {
		return "", "", fmt.Errorf("cannot derive job name from pod %q", podName)
	}
	jobName := podName[:i]

	if !strings.HasPrefix(jobName, jobNamePrefix) {
		return "", "", fmt.Errorf("unexpected job name %q (want prefix %q)", jobName, jobNamePrefix)
	}
	local := strings.TrimPrefix(jobName, jobNamePrefix)

	switch local {
	case n1:
		return local, n2, nil
	case n2:
		return local, n1, nil
	default:
		return "", "", fmt.Errorf("local %q not in cluster config (%q,%q)", local, n1, n2)
	}
}

func waitPeerValidateIfSecond(ctx context.Context, kc kubernetes.Interface, local, a, b string) error {
	min, max := a, b
	if strings.Compare(min, max) > 0 {
		min, max = max, min
	}
	if local != max {
		return nil
	}
	target := tools.JobTypeDisruptiveValidate.GetJobName(&min)
	missingSince := time.Time{}
	return wait.PollUntilContextTimeout(ctx, poll, timeoutPeerJob, true, func(context.Context) (bool, error) {
		j, err := kc.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, target, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if missingSince.IsZero() {
				missingSince = time.Now()
			} else if time.Since(missingSince) > 2*time.Minute {
				return false, fmt.Errorf("peer job %s not found for >2m; likely GC'd or never created", target)
			}
			return false, nil
		}
		if err != nil {
			return false, nil
		}
		if j.Status.Failed > 0 || tools.IsConditionTrue(j.Status.Conditions, batchv1.JobFailed) {
			return false, fmt.Errorf("peer validate job %s failed", target)
		}
		return j.Status.Succeeded > 0 || tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete), nil
	})
}

func waitEtcdTwoVoters(ctx context.Context, to time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, poll, to, true, func(context.Context) (bool, error) {
		out, _, err := exec.Execute(ctx, `podman exec etcd sh -lc 'ETCDCTL_API=3 etcdctl member list -w json'`)
		out = strings.TrimSpace(out)
		if err != nil || len(out) < 2 {
			return false, nil
		}
		var ml struct {
			Members []struct {
				IsLearner bool `json:"isLearner"`
			} `json:"members"`
		}
		if json.Unmarshal([]byte(out), &ml) != nil {
			return false, nil
		}
		total, voters := 0, 0
		for _, m := range ml.Members {
			total++
			if !m.IsLearner {
				voters++
			}
		}
		return total == 2 && voters == 2, nil
	})
}

func waitPCSState(ctx context.Context, want *bool, peer string, to time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, poll, to, true, func(context.Context) (bool, error) {
		out, _, err := exec.Execute(ctx, `/usr/sbin/pcs status`)
		if err != nil {
			return false, nil
		}
		online := map[string]bool{}
		for _, ln := range strings.Split(out, "\n") {
			s := strings.TrimSpace(strings.TrimPrefix(ln, "*"))
			// Format A: "Node <name> state: ONLINE|OFFLINE"
			if strings.HasPrefix(s, "Node ") && strings.Contains(s, " state: ") {
				fs := strings.Fields(s)
				if len(fs) >= 4 {
					state := fs[len(fs)-1]
					online[fs[1]] = (state == "ONLINE")
				}
				continue
			}
			// Format B: "Online: n1 n2 ..." / "Offline: nX ..."
			if strings.HasPrefix(s, "Online:") {
				for _, n := range strings.Fields(s[len("Online:"):]) {
					online[strings.TrimSpace(n)] = true
				}
			} else if strings.HasPrefix(s, "Offline:") {
				for _, n := range strings.Fields(s[len("Offline:"):]) {
					if _, seen := online[strings.TrimSpace(n)]; !seen {
						online[strings.TrimSpace(n)] = false
					}
				}
			}
		}
		if want == nil {
			return true, nil
		}
		on, ok := online[peer]
		return ok && on == *want, nil
	})
}

func boolp(b bool) *bool { return &b }

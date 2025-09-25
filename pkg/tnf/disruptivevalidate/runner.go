package disruptivevalidate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	clusterCfg, err := config.GetClusterConfig(ctx, kc)
	if err != nil {
		return err
	}
	local, peer, err := detectLocalAndPeer(clusterCfg.NodeName1, clusterCfg.NodeName2)
	if err != nil {
		return err
	}
	klog.Infof("validate: local=%s peer=%s", local, peer)

	if err := waitForLabeledJob(ctx, kc, tools.JobTypeFencing.GetNameLabelValue(), 1, timeoutFencing); err != nil {
		return fmt.Errorf("fencing not complete: %w", err)
	}
	if err := waitForJobName(ctx, kc, tools.JobTypeAfterSetup.GetJobName(&clusterCfg.NodeName1), timeoutAfter); err != nil {
		return fmt.Errorf("after-setup not complete on %s: %w", clusterCfg.NodeName1, err)
	}
	if err := waitForJobName(ctx, kc, tools.JobTypeAfterSetup.GetJobName(&clusterCfg.NodeName2), timeoutAfter); err != nil {
		return fmt.Errorf("after-setup not complete on %s: %w", clusterCfg.NodeName2, err)
	}

	// 2) Lexicographic sequencing
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
func waitForJobs(
	ctx context.Context,
	kc kubernetes.Interface,
	byName string,
	labelSelector string,
	wantAtLeast int,
	to time.Duration,
	allowNeverSeenTTL bool, // NEW
) error {
	const appearanceGrace = 2 * time.Minute
	start := time.Now()
	seen := false

	return wait.PollUntilContextTimeout(ctx, poll, to, true, func(context.Context) (bool, error) {
		if byName != "" {
			j, err := kc.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, byName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				if seen {
					klog.V(2).Infof("job %s disappeared after observation; assuming TTL after completion", byName)
					return true, nil
				}
				if allowNeverSeenTTL && time.Since(start) > appearanceGrace {
					klog.V(2).Infof("job %s not found for %s; assuming completed earlier and TTL-deleted", byName, appearanceGrace)
					return true, nil
				}
				return false, nil
			}
			if err != nil {
				return false, nil // transient
			}

			seen = true

			if tools.IsConditionTrue(j.Status.Conditions, batchv1.JobFailed) {
				return false, fmt.Errorf("job %s failed", byName)
			}
			return j.Status.Succeeded > 0 || tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete), nil
		}

		// selector path (aggregate waits)
		jl, err := kc.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, nil // transient
		}
		if len(jl.Items) < wantAtLeast {
			return false, nil
		}
		completed := 0
		for i := range jl.Items {
			j := &jl.Items[i]
			if tools.IsConditionTrue(j.Status.Conditions, batchv1.JobFailed) {
				return false, fmt.Errorf("job %s failed", j.Name)
			}
			if j.Status.Succeeded > 0 || tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete) {
				completed++
			}
		}
		return completed >= wantAtLeast, nil
	})
}

func waitForLabeledJob(ctx context.Context, kc kubernetes.Interface, nameLabel string, wantAtLeast int, to time.Duration) error {
	return waitForJobs(ctx, kc,
		"", // byName
		fmt.Sprintf("app.kubernetes.io/name=%s", nameLabel),
		wantAtLeast, to, false) // strict
}

func waitForJobName(ctx context.Context, kc kubernetes.Interface, name string, to time.Duration) error {
	return waitForJobs(ctx, kc,
		name,         // byName
		"",           // labelSelector
		1, to, false) // strict
}

func waitForJobNamePeerTTL(ctx context.Context, kc kubernetes.Interface, name string, to time.Duration) error {
	return waitForJobs(ctx, kc, name, "", 1, to, true) // allowNeverSeenTTL = true
}

func detectLocalAndPeer(n1, n2 string) (string, string, error) {
	podName, err := os.Hostname()
	if err != nil {
		return "", "", fmt.Errorf("get pod hostname: %w", err)
	}
	podName = strings.TrimSpace(podName)
	if podName == "" {
		return "", "", fmt.Errorf("get pod hostname: empty string")
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
	if err := waitForJobNamePeerTTL(ctx, kc, target, timeoutPeerJob); err != nil {
		return fmt.Errorf("peer validate job %s not complete: %w", min, err)
	}
	return nil
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
		out, _, err := exec.Execute(ctx, `LC_ALL=C /usr/sbin/pcs status nodes`)
		if err != nil {
			return false, nil // treat as transient
		}

		// Find the "Online:" line and build a set of names listed there.
		var onlineLine string
		for _, ln := range strings.Split(out, "\n") {
			s := strings.TrimSpace(ln)
			if strings.HasPrefix(s, "Online:") {
				onlineLine = strings.TrimSpace(strings.TrimPrefix(s, "Online:"))
				break
			}
		}
		peerOnline := false
		if onlineLine != "" {
			for _, tok := range strings.Fields(onlineLine) {
				if strings.Trim(tok, "[],") == peer {
					peerOnline = true
					break
				}
			}
		}
		return peerOnline == *want, nil
	})
}

func boolp(b bool) *bool { return &b }

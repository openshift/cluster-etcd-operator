package disruptivevalidate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type etcdMembers struct {
	Members []struct {
		Name      string `json:"name"`
		IsLearner bool   `json:"isLearner"`
	} `json:"members"`
}

func RunDisruptiveValidate() error {
	klog.Info("Setting up clients for TNF validate job")

	ctx, cancel := context.WithCancel(context.Background())
	shutdown := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdown
		klog.Info("Received termination signal, exiting validate job")
	}()

	// kube client
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	// Wait for SETUP (cluster-wide) to complete
	klog.Info("Waiting for completed setup job before validation")
	setupDone := func(context.Context) (bool, error) {
		jobs, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeSetup.GetNameLabelValue()),
		})
		if err != nil || len(jobs.Items) != 1 {
			klog.Warningf("setup job not ready yet, err=%v count=%d", err, len(jobs.Items))
			return false, nil
		}
		if !tools.IsConditionTrue(jobs.Items[0].Status.Conditions, batchv1.JobComplete) {
			return false, nil
		}
		return true, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.SetupJobCompletedTimeout, true, setupDone); err != nil {
		return fmt.Errorf("waiting for setup to complete: %w", err)
	}
	// wait for FENCING (cluster-wide) to complete
	klog.Info("Waiting for completed fencing job before validation")
	fencingDone := func(context.Context) (bool, error) {
		jobs, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeFencing.GetNameLabelValue()),
		})
		if err != nil || len(jobs.Items) != 1 {
			klog.Warningf("fencing job not ready yet, err=%v count=%d", err, len(jobs.Items))
			return false, nil
		}
		if !tools.IsConditionTrue(jobs.Items[0].Status.Conditions, batchv1.JobComplete) {
			return false, nil
		}
		return true, nil
	}
	_ = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.FencingJobCompletedTimeout, true, fencingDone)

	// wait for BOTH AFTER-SETUP (per-node) jobs to complete
	klog.Info("Waiting for completed after-setup jobs before validation")
	afterSetupDone := func(context.Context) (bool, error) {
		jobs, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeAfterSetup.GetNameLabelValue()),
		})
		if err != nil || len(jobs.Items) != 2 {
			klog.Warningf("after-setup jobs not ready yet, err=%v count=%d", err, len(jobs.Items))
			return false, nil
		}
		for _, j := range jobs.Items {
			if !tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete) {
				return false, nil
			}
		}
		return true, nil
	}
	_ = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.SetupJobCompletedTimeout, true, afterSetupDone)

	// Discover cluster config (node names)
	clusterCfg, err := config.GetClusterConfig(ctx, kubeClient)
	if err != nil {
		return err
	}

	// Determine which node this pod is running on
	podName, err := os.Hostname() // pod name == container hostname
	if err != nil || strings.TrimSpace(podName) == "" {
		return fmt.Errorf("get pod hostname/name: %w", err)
	}
	pod, err := kubeClient.CoreV1().Pods(operatorclient.TargetNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Pod %s/%s: %w", operatorclient.TargetNamespace, podName, err)
	}
	local := strings.TrimSpace(pod.Spec.NodeName)
	if local == "" {
		return fmt.Errorf("pod.Spec.NodeName empty")
	}

	var peer string
	switch local {
	case clusterCfg.NodeName1:
		peer = clusterCfg.NodeName2
	case clusterCfg.NodeName2:
		peer = clusterCfg.NodeName1
	default:
		return fmt.Errorf("host %q not in cluster config (%q, %q)", local, clusterCfg.NodeName1, clusterCfg.NodeName2)
	}

	klog.Infof("TNF validate: local=%s peer=%s", local, peer)

	min, max := clusterCfg.NodeName1, clusterCfg.NodeName2
	if strings.Compare(min, max) > 0 {
		min, max = max, min
	}
	waitEtcdVoters := func(timeout time.Duration) error {
		check := func(context.Context) (bool, error) {
			out, _, err := exec.Execute(ctx, `podman exec etcd sh -lc 'ETCDCTL_API=3 etcdctl member list -w json'`)
			if err != nil || strings.TrimSpace(out) == "" {
				return false, nil
			}
			var ml etcdMembers
			if e := json.Unmarshal([]byte(out), &ml); e != nil {
				return false, nil
			}
			total, voters := 0, 0
			for _, m := range ml.Members {
				total++
				if !m.IsLearner {
					voters++
				}
			}
			// Require exactly 2 members and both voters
			return total == 2 && voters == 2, nil
		}
		return wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, check)
	}
	// If "second" node (max), wait for the "first" node's validate Job to Complete.
	if local == max {
		targetJobName := tools.JobTypeDisruptiveValidate.GetJobName(&min) // e.g., tnf-disruptive-validate-job-master-0
		klog.Infof("validate: %s waiting for %s to complete (job=%s)", local, min, targetJobName)

		// Robust job state helpers
		isJobComplete := func(j *batchv1.Job) bool {
			if j.Status.Succeeded > 0 {
				return true
			}
			return tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete)
		}
		isJobFailed := func(j *batchv1.Job) bool {
			if j.Status.Failed > 0 {
				return true
			}
			return tools.IsConditionTrue(j.Status.Conditions, batchv1.JobFailed)
		}

		err := wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, 45*time.Minute, true, func(context.Context) (bool, error) {
			j, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, targetJobName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				klog.Infof("peer job %s not found yet; still waiting", targetJobName)
				return false, nil
			}
			if err != nil {
				klog.Warningf("peer job get error: %v", err)
				return false, nil // transient
			}
			if isJobFailed(j) {
				return false, fmt.Errorf("peer validate job %s failed (failed=%d)", targetJobName, j.Status.Failed)
			}
			return isJobComplete(j), nil
		})
		if err != nil {
			return fmt.Errorf("timed out waiting for %s (%s) to complete: %w", min, targetJobName, err)
		}

		if err := waitEtcdVoters(10 * time.Minute); err != nil {
			return fmt.Errorf("etcd members not both voters after first validate: %w", err)
		}
		klog.Infof("validate: %s saw %s complete + etcd OK; proceeding to fence", local, min)
	}

	// Preflight on host
	if _, _, err := exec.Execute(ctx, `command -v pcs`); err != nil {
		return fmt.Errorf("pcs absent on host: %w", err)
	}
	if _, _, err := exec.Execute(ctx, `systemctl is-active pacemaker`); err != nil {
		return fmt.Errorf("pacemaker not active: %w", err)
	}

	// waiter for both peer state
	waitPCS := func(wantPeer *bool, peer string, timeout time.Duration) error {
		// wantPeer: nil = don't check peer, &true = want ONLINE, &false = want OFFLINE
		reNodeLine := regexp.MustCompile(`(?mi)^\s*Node\s+(\S+)\s+state:\s+([A-Z]+)`)
		reOnline := regexp.MustCompile(`(?mi)^\s*(?:\*\s*)?Online:\s*(.*)$`)
		check := func(context.Context) (bool, error) {
			out, _, err := exec.Execute(ctx, `/usr/sbin/pcs status`)
			if err != nil {
				return false, nil // transient
			}
			// --- peer ONLINE set ---
			online := map[string]bool{}
			// Format A: per-node lines
			for _, m := range reNodeLine.FindAllStringSubmatch(out, -1) {
				if len(m) == 3 {
					online[m[1]] = (m[2] == "ONLINE")
				}
			}
			// Format B: summary list
			if m := reOnline.FindStringSubmatch(out); len(m) == 2 {
				for _, n := range strings.Fields(m[1]) {
					online[n] = true
				}
			}
			// Conditions
			if wantPeer != nil {
				if on, ok := online[peer]; !ok || on != *wantPeer {
					return false, nil
				}
			}
			return true, nil
		}
		return wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, check)
	}

	if err := waitPCS(func() *bool { b := true; return &b }(), peer, 2*time.Minute); err != nil {
		return fmt.Errorf("peer %q not ONLINE pre-fence: %w", peer, err)
	}

	// Fence peer
	if out, _, err := exec.Execute(ctx, fmt.Sprintf(`/usr/sbin/pcs stonith fence %s`, peer)); err != nil {
		last := out
		if nl := strings.LastIndex(out, "\n"); nl >= 0 && nl+1 < len(out) {
			last = out[nl+1:]
		}
		return fmt.Errorf("pcs fence failed: %w (last: %s)", err, strings.TrimSpace(last))
	}

	// Wait OFFLINE then ONLINE
	if err := waitPCS(func() *bool { b := false; return &b }(), peer, 5*time.Minute); err != nil {
		return fmt.Errorf("peer didn't go OFFLINE: %w", err)
	}

	if err := waitPCS(func() *bool { b := true; return &b }(), peer, 10*time.Minute); err != nil {
		return fmt.Errorf("peer didn't become ONLINE with etcd started on both: %w", err)
	}

	klog.Infof("TNF validate: success local=%s peer=%s", local, peer)
	return nil
}

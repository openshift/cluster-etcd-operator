package disruptivevalidate

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
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
	_ = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.SetupJobCompletedTimeout, true, setupDone)

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

	// Determine which host this pod is on (nsenter wrapper runs on host)
	hostOut, _, err := exec.Execute(ctx, "hostname")
	if err != nil {
		return fmt.Errorf("get host hostname: %w", err)
	}
	local := strings.TrimSpace(hostOut)

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
	waitEtcdVoters := func(nameA, nameB string, timeout time.Duration) error {
		check := func(context.Context) (bool, error) {
			out, _, err := exec.Execute(ctx, `podman exec etcd sh -lc 'ETCDCTL_API=3 etcdctl member list -w json'`)
			if err != nil || strings.TrimSpace(out) == "" {
				return false, nil
			}
			var ml etcdMembers
			if e := json.Unmarshal([]byte(out), &ml); e != nil {
				return false, nil
			}
			seen := map[string]bool{}
			voter := map[string]bool{}
			for _, m := range ml.Members {
				seen[m.Name] = true
				voter[m.Name] = !m.IsLearner
			}
			// require both members present and both voters
			return seen[nameA] && seen[nameB] && voter[nameA] && voter[nameB], nil
		}
		return wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, check)
	}
	// If "second" node (max), wait for the "first" node's validate Job to Complete.
	if local == max {
		targetJobName := tools.JobTypeDisruptiveValidate.GetJobName(&min) // e.g. tnf-disruptive-validate-job-<min>
		klog.Infof("validate: %s waiting for %s to complete (%s)", local, min, targetJobName)

		err := wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, 45*time.Minute, true, func(context.Context) (bool, error) {
			j, err := kubeClient.BatchV1().Jobs(operatorclient.TargetNamespace).Get(ctx, targetJobName, metav1.GetOptions{})
			if err != nil {
				// NotFound or transient—keep polling
				return false, nil
			}
			return tools.IsConditionTrue(j.Status.Conditions, batchv1.JobComplete), nil
		})
		if err != nil {
			return fmt.Errorf("timed out waiting for %s (%s) to complete: %w", min, targetJobName, err)
		}

		if err := waitEtcdVoters(clusterCfg.NodeName1, clusterCfg.NodeName2, 10*time.Minute); err != nil {
			return fmt.Errorf("etcd members did not start or both not voters yet; refusing second fence: %w", err)
		}
		klog.Infof("validate: %s saw %s complete; proceeding to fence", local, min)
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

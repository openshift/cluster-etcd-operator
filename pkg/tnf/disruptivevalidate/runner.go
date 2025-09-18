package fencingvalidate

import (
	"context"
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

	// NEW: wait for FENCING (cluster-wide) to complete
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

	// NEW: wait for BOTH AFTER-SETUP (per-node) jobs to complete
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

	// Preflight on host
	if _, _, err := exec.Execute(ctx, `command -v pcs`); err != nil {
		return fmt.Errorf("pcs absent on host: %w", err)
	}
	if _, _, err := exec.Execute(ctx, `systemctl is-active pacemaker`); err != nil {
		return fmt.Errorf("pacemaker not active: %w", err)
	}

	// Ensure peer ONLINE before fence
	peerOnlineRE := regexp.MustCompile(`(?mi)^Node\s+` + regexp.QuoteMeta(peer) + `\s+state:\s+([A-Z]+)`)
	waitPeer := func(wantOnline bool, timeout time.Duration) error {
		check := func(context.Context) (bool, error) {
			out, _, err := exec.Execute(ctx, `/usr/sbin/pcs status nodes`)
			if err != nil {
				// transient during fencing
				return false, nil
			}
			m := peerOnlineRE.FindStringSubmatch(out)
			if len(m) != 2 {
				return false, nil
			}
			gotOnline := (m[1] == "ONLINE")
			return gotOnline == wantOnline, nil
		}
		return wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, check)
	}
	if err := waitPeer(true, 2*time.Minute); err != nil {
		return fmt.Errorf("peer %q not ONLINE pre-fence: %w", peer, err)
	}

	// Fence peer
	if _, _, err := exec.Execute(ctx, fmt.Sprintf(`/usr/sbin/pcs stonith fence %s --wait=300`, peer)); err != nil {
		return fmt.Errorf("pcs fence failed: %w", err)
	}

	// Wait OFFLINE then ONLINE
	if err := waitPeer(false, 10*time.Minute); err != nil {
		return fmt.Errorf("peer didn't go OFFLINE: %w", err)
	}
	if err := waitPeer(true, 15*time.Minute); err != nil {
		return fmt.Errorf("peer didn't become ONLINE: %w", err)
	}

	klog.Infof("TNF validate: success local=%s peer=%s", local, peer)
	return nil
}

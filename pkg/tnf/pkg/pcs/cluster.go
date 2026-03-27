package pcs

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// ConfigureCluster checks the pcs cluster status and runs the finalization script if needed
func ConfigureCluster(ctx context.Context, cfg config.ClusterConfig) (bool, error) {
	klog.Info("Checking pcs cluster status")

	// check how far we are...
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs resource config")

	if strings.Contains(stdOut, "kubelet-clone") {
		// all done already
		klog.Infof("HA cluster setup already done")
		return false, nil
	}

	klog.Info("Starting initial HA cluster setup")

	commands := []string{
		fmt.Sprintf("/usr/sbin/pcs cluster setup TNF %s addr=%s %s addr=%s --debug --force", cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2),
		"/usr/sbin/pcs cluster start --all",
		"/usr/sbin/pcs property set start-failure-is-fatal=false",
		// Note: the kubelet service needs to be disabled when using systemd agent
		// Done by after-setup jobs on both nodes
		// Note: `migration-threshold=60` caps Pacemaker start failures before the resource is treated as blocked (60 retries).
		// Note: `op start start-delay=15s` waits 15s before each start attempt so dependencies (e.g. resolv prepender) can settle; fast-fail ExecConditions still return quickly.
		// Note: `op start timeout=180s` bounds how long a single start may run; it does not add delay between retries (that is start-delay).
		"/usr/sbin/pcs resource create kubelet systemd:kubelet op start timeout=180s start-delay=15s clone meta interleave=true migration-threshold=60",
		"/usr/sbin/pcs cluster enable --all",
		"/usr/sbin/pcs cluster sync",
		"/usr/sbin/pcs cluster reload corosync",
	}

	for _, command := range commands {
		stdOut, stdErr, err = exec.Execute(ctx, command)
		if err != nil {
			klog.Error(err, "Failed to run HA setup command", "command", command, "stdout", stdOut, "stderr", stdErr, "err", err)
			return false, err
		}
	}
	klog.Info("Initial HA cluster config done!")
	return true, nil

}

// GetCIB gets the pcs cib
func GetCIB(ctx context.Context) (string, error) {
	klog.Info("Getting pcs cib")
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs cluster cib")
	// redact passwords!
	stdOut = tools.RedactPasswords(stdOut)
	if err != nil || len(stdErr) > 0 {
		klog.Error(err, "Failed to get final pcs cib", "stdout", stdOut, "stderr", stdErr, "err", err)
		return "", err
	}
	return stdOut, nil
}

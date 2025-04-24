package pcs

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

// ConfigureEtcd configures the etcd resource
func ConfigureEtcd(ctx context.Context, cfg config.ClusterConfig) error {
	klog.Info("Checking pcs resources")

	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs resource status")
	if err != nil || len(stdErr) > 0 {
		klog.Error(err, "Failed to get pcs resource status", "stdout", stdOut, "stderr", stdErr, "err", err)
		return err
	}
	if !strings.Contains(stdOut, "etcd") {
		klog.Info("Creating etcd resource")
		cmd := fmt.Sprintf("/usr/sbin/pcs resource create etcd podman-etcd node_ip_map=\"%s:%s;%s:%s\" drop_in_dependency=true clone interleave=true notify=true",
			cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2)
		stdOut, stdErr, err = exec.Execute(ctx, cmd)
		if err != nil || len(stdErr) > 0 {
			klog.Error(err, "Failed to create etcd resource", "stdout", stdOut, "stderr", stdErr, "err", err)
			return err
		}
	}
	return nil
}

// ConfigureConstraints configures the etcd constraints
func ConfigureConstraints(ctx context.Context) (bool, error) {
	klog.Info("Checking pcs constraints")
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs constraint")
	if err != nil || len(stdErr) > 0 {
		klog.Error(err, "Failed to get pcs resource status", "stdout", stdOut, "stderr", stdErr, "err", err)
		return false, err
	}
	if !strings.Contains(stdOut, "etcd") {
		klog.Info("Configuring etcd constraints")
		stdOut, stdErr, err = exec.Execute(ctx, "/usr/sbin/pcs constraint order kubelet-clone then etcd-clone && /usr/sbin/pcs constraint colocation add etcd-clone with kubelet-clone")
		if err != nil || len(stdErr) > 0 {
			klog.Error(err, "Failed to configure etcd constraints", "stdout", stdOut, "stderr", stdErr, "err", err)
			return false, err
		}
		return true, nil
	}
	return false, nil
}

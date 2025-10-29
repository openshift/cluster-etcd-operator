package kubelet

import (
	"context"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

func Disable(ctx context.Context) error {
	klog.Info("Disabling kubelet service")
	command := "systemctl disable kubelet"
	_, _, err := exec.Execute(ctx, command)
	return err
}

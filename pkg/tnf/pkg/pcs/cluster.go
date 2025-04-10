package pcs

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
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

	//stonithArgsNode1 := getStonithArgs(cfg, 0)
	//stonithArgsNode2 := getStonithArgs(cfg, 1)

	commands := []string{
		fmt.Sprintf("/usr/sbin/pcs cluster setup TNF %s addr=%s %s addr=%s --debug --force", cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2),
		"/usr/sbin/pcs cluster start --all",
		//fmt.Sprintf("/usr/sbin/pcs stonith create %s", stonithArgsNode1),
		//fmt.Sprintf("/usr/sbin/pcs stonith create %s", stonithArgsNode2),
		// !!! TODO REMOVE WHEN ENABLING STONITH !!!
		"/usr/sbin/pcs property set stonith-enabled=false",
		"/usr/sbin/pcs resource create kubelet systemctl service=kubelet clone meta interleave=true",
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

//func getStonithArgs(cfg config.ClusterConfig, nodeNr int) string {
//	fc := cfg.FencingConfigs[nodeNr]
//	args := fmt.Sprintf("%s %s", fc.FencingID, fc.FencingDeviceType)
//	for key, value := range fc.FencingDeviceOptions {
//		if len(value) == 0 {
//			value = "1"
//		}
//		args += fmt.Sprintf(` %s="%s"`, key, value)
//	}
//	return args
//}

// GetCIB gets the pcs cib
func GetCIB(ctx context.Context) (string, error) {
	klog.Info("Getting pcs cib")
	stdOut, stdErr, err := exec.Execute(ctx, "/usr/sbin/pcs cluster cib")
	if err != nil || len(stdErr) > 0 {
		klog.Error(err, "Failed to get final pcs cib", "stdout", stdOut, "stderr", stdErr, "err", err)
		return "", err
	}
	return stdOut, nil
}

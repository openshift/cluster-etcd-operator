package updatesetup

import (
	"context"
	"fmt"
	"os"
	"strings"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

func RunTnfUpdateSetup() error {

	klog.Info("Setting up clients etc. for TNF update-setup")

	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	operatorClient, dynamicInformers, err := genericoperatorclient.NewStaticPodOperatorClient(clock.RealClock{}, clientConfig, operatorv1.GroupVersion.WithResource("etcds"), operatorv1.GroupVersion.WithKind("Etcd"), operator.ExtractStaticPodOperatorSpec, operator.ExtractStaticPodOperatorStatus)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	dynamicInformers.Start(ctx.Done())
	dynamicInformers.WaitForCacheSync(ctx.Done())

	// Get the current node name from environment
	currentNodeName := os.Getenv("MY_NODE_NAME")
	if currentNodeName == "" {
		return fmt.Errorf("MY_NODE_NAME environment variable not set")
	}

	klog.Infof("Running TNF update-setup")

	// check if cluster is running on this node
	command := "/usr/sbin/pcs cluster status"
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		klog.Infof("Cluster not running (err: %v), skipping update-setup on this node", err)
		return nil
	}

	// Get current cluster config from Kubernetes
	cfg, err := config.GetClusterConfig(ctx, kubeClient)
	if err != nil {
		return err
	}

	// TODO move to pgk/pcs/cluster.go

	// Determine which node we are and get the new IP
	var currentNodeIP, otherNodeName, otherNodeIP string
	if cfg.NodeName1 == currentNodeName {
		currentNodeIP = cfg.NodeIP1
		otherNodeName = cfg.NodeName2
		otherNodeIP = cfg.NodeIP2
	} else if cfg.NodeName2 == currentNodeName {
		currentNodeIP = cfg.NodeIP2
		otherNodeName = cfg.NodeName1
		otherNodeIP = cfg.NodeIP1
	} else {
		return fmt.Errorf("current node %s not found in cluster config (nodes: %s, %s)", currentNodeName, cfg.NodeName1, cfg.NodeName2)
	}

	// find offline node
	command = "/usr/sbin/pcs status nodes corosync | grep Offline | awk '{print $2}'"
	stdOut, stdErr, err := exec.Execute(ctx, command)
	if err != nil {
		klog.Errorf("Failed to find offline node: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
		return err
	}
	offlineNodeName := strings.TrimSpace(stdOut)

	klog.Infof("Current node: %q (IP: %s), Other node: %q (IP: %s), Offline node: %q", currentNodeName, currentNodeIP, otherNodeName, otherNodeIP, offlineNodeName)

	// don't start the cluster on the new node too early, it might result in etcd start failure because of missing manifests on the new node
	err = etcd.WaitForUpdatedRevision(ctx, operatorClient)
	if err != nil {
		klog.Error(err, "Failed to wait for etcd container transition")
		return err
	}

	if offlineNodeName != "" {
		// add new node and update etcd resource
		commands := []string{
			// Remove offline node from the cluster configuration
			fmt.Sprintf("/usr/sbin/pcs cluster node remove %s --force --skip-offline", offlineNodeName),
			// Add new node to the cluster configuration
			fmt.Sprintf("/usr/sbin/pcs cluster node add %s", otherNodeName),
			// update etcd resource
			fmt.Sprintf("/usr/sbin/pcs resource update etcd node_ip_map=\"%s:%s;%s:%s\"", cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2),
		}
		err = runCommands(commands, ctx)
		if err != nil {
			return err
		}
	}

	// always start cluster
	commands := []string{
		// Start cluster on new node
		"/usr/sbin/pcs cluster enable --all",
		"/usr/sbin/pcs cluster start --all",
	}
	err = runCommands(commands, ctx)
	if err != nil {
		return err
	}

	return nil
}

func runCommands(commands []string, ctx context.Context) error {
	for _, command := range commands {
		stdOut, stdErr, err := exec.Execute(ctx, command)
		if err != nil {
			klog.Errorf("Failed to run update-setup command: %s, stdout: %s, stderr: %s, err: %v", command, stdOut, stdErr, err)
			return err
		}
		klog.Infof("Successfully executed: %s", command)
	}
	return nil
}

package pcs

import (
	"context"
	"fmt"

	"github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
)

const (
	TokenPath = "/var/lib/pcsd/token"
)

// Authenticate creates a token file and authenticates the node with the pacemaker cluster
func Authenticate(ctx context.Context, configClient *versioned.Clientset, cfg config.ClusterConfig) (bool, error) {

	// use cluster id as immutable token
	clusterVersion, err := configClient.ConfigV1().ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get cluster version: %v", err)
		return false, err
	}
	tokenValue := clusterVersion.Spec.ClusterID

	command := fmt.Sprintf("echo %q > %s", tokenValue, TokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return false, fmt.Errorf("failed to create token file: %w", err)
	}

	command = fmt.Sprintf("chmod 0600 %s", TokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return false, fmt.Errorf("failed to set permissions on token file: %w", err)
	}

	// run pcs auth
	command = fmt.Sprintf("/usr/sbin/pcs pcsd accept_token %s", TokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return false, fmt.Errorf("failed to accept token: %w", err)
	}

	command = fmt.Sprintf("/usr/sbin/pcs host auth %s addr=%s %s addr=%s --token %s --debug", cfg.NodeName1, cfg.NodeIP1, cfg.NodeName2, cfg.NodeIP2, TokenPath)
	_, _, err = exec.Execute(ctx, command)
	if err != nil {
		return false, fmt.Errorf("failed to authenticate node: %w", err)
	}

	return true, nil
}

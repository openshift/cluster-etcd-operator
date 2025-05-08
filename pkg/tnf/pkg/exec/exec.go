package exec

import (
	"context"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

// Execute executes the command
func Execute(ctx context.Context, command string) (stdout, stderr string, err error) {

	// This runs the given commmand in the hosts root namespace.
	// This currently is the best way to run the `pcs` commands on the node,
	// until it supports a better client - server architecture.
	// This needs to run as `privileged` and with `hostPID: true`.
	hostCommand := []string{"/usr/bin/nsenter", "-a", "-t 1", "/bin/bash", "-c"}
	hostCommand = append(hostCommand, command)

	// redact passwords in the command log
	klog.Infof("Executing: %s", tools.RedactPasswords(strings.Join(hostCommand, " ")))

	cmd := exec.CommandContext(ctx, hostCommand[0], hostCommand[1:]...)

	var outBuilder, errBuilder strings.Builder
	cmd.Stdout = &outBuilder
	cmd.Stderr = &errBuilder

	err = cmd.Run()

	stdout = outBuilder.String()
	stderr = errBuilder.String()

	// redact passwords in logs, but not in return values!
	klog.Infof("  stdout: %s", tools.RedactPasswords(stdout))
	klog.Infof("  stderr: %s", tools.RedactPasswords(stderr))
	if err != nil {
		klog.Infof("  err: %s", tools.RedactPasswords(err.Error()))
	} else {
		klog.Infof("  err: <nil>")
	}

	return
}

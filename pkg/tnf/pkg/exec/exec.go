package exec

import (
	"context"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
)

// Execute executes the command
func Execute(ctx context.Context, command string) (stdout, stderr string, err error) {

	// This runs the given commmand in the hosts root namespace.
	// This currently is the best way to run the `pcs` commands on the node,
	// until it supports a better client - server architecture.
	// This needs to run as `privileged` and with `hostPID: true`.
	hostCommand := []string{"/usr/bin/nsenter", "-a", "-t 1", "/bin/bash", "-c"}
	hostCommand = append(hostCommand, command)

	klog.Infof("Executing: %s", strings.Join(hostCommand, " "))

	cmd := exec.CommandContext(ctx, hostCommand[0], hostCommand[1:]...)

	var outBuilder, errBuilder strings.Builder
	cmd.Stdout = &outBuilder
	cmd.Stderr = &errBuilder

	err = cmd.Run()

	klog.Infof("  stdout: %s", outBuilder.String())
	klog.Infof("  stderr: %s", errBuilder.String())
	klog.Infof("  err: %v", err)

	return outBuilder.String(), errBuilder.String(), err
}

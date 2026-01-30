package main

import (
	goflag "flag"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/grpclog"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
)

func main() {
	// overwrite gRPC logger, to discard all gRPC info-level logging
	// https://github.com/kubernetes/kubernetes/issues/80741
	// https://github.com/kubernetes/kubernetes/pull/84061
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, os.Stderr, os.Stderr))

	logs.InitLogs()
	defer logs.FlushLogs()

	command := NewTnfMonitorCommand()
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func NewTnfMonitorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tnf-monitor",
		Short: "OpenShift Two Node Fencing Monitor",
		Long:  "Monitors and collects pacemaker cluster status for Two Node Fencing deployments",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
			os.Exit(1)
		},
	}

	// Register logging flags on persistent flags so they're available to all subcommands
	cmd.PersistentFlags().SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	cmd.PersistentFlags().AddGoFlagSet(goflag.CommandLine)
	logs.AddFlags(cmd.PersistentFlags())

	cmd.AddCommand(NewCollectCommand())

	return cmd
}

func NewCollectCommand() *cobra.Command {
	// Reuse the existing collector command as the "collect" subcommand.
	cmd := pacemaker.NewPacemakerStatusCollectorCommand()
	cmd.Use = "collect"
	cmd.Short = "Collect pacemaker cluster status"
	cmd.Long = "Collects pacemaker status and updates PacemakerCluster CR"
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd
}

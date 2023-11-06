package main

import (
	"context"
	goflag "flag"
	"fmt"
	"os"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/hypershift"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
)

func main() {
	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	logs.AddFlags(pflag.CommandLine)
	logs.InitLogs()
	defer logs.FlushLogs()

	command := NewCommand(context.Background())
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func NewCommand(ctx context.Context) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hypershift-etcd-operator",
		Short: "Hypershift etcd operator",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
			os.Exit(1)
		},
	}

	cmd.AddCommand(hypershift.NewHypershiftOperator())

	return cmd
}

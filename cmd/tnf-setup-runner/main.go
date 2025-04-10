package main

import (
	goflag "flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/grpclog"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"

	tnfauth "github.com/openshift/cluster-etcd-operator/pkg/tnf/auth"
	tnfsetup "github.com/openshift/cluster-etcd-operator/pkg/tnf/setup"
)

func main() {
	// overwrite gRPC logger, to discard all gRPC info-level logging
	// https://github.com/kubernetes/kubernetes/issues/80741
	// https://github.com/kubernetes/kubernetes/pull/84061
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(ioutil.Discard, os.Stderr, os.Stderr))

	rand.Seed(time.Now().UTC().UnixNano())

	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	logs.AddFlags(pflag.CommandLine)
	logs.InitLogs()
	defer logs.FlushLogs()

	command := NewTnfSetupRunnerCommand()
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func NewTnfSetupRunnerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tnf-setup-runner",
		Short: "OpenShift Two Node Fencing Setup runner",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
			os.Exit(1)
		},
	}

	cmd.AddCommand(NewAuthCommand())
	cmd.AddCommand(NewRunCommand())

	return cmd
}

func NewAuthCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "auth",
		Short: "Run Two Node Fencing pcs authentication",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnfauth.RunTnfAuth()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

func NewRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the Two Node Fencing setup",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnfsetup.RunTnfSetup()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

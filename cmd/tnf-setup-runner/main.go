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

	tnfaftersetup "github.com/openshift/cluster-etcd-operator/pkg/tnf/after-setup"
	tnfauth "github.com/openshift/cluster-etcd-operator/pkg/tnf/auth"
	tnffencing "github.com/openshift/cluster-etcd-operator/pkg/tnf/fencing"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	tnfrestartetcd "github.com/openshift/cluster-etcd-operator/pkg/tnf/restart-etcd"
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
	cmd.AddCommand(NewSetupCommand())
	cmd.AddCommand(NewAfterSetupCommand())
	cmd.AddCommand(NewFencingCommand())
	cmd.AddCommand(NewRestartEtcdCommand())

	return cmd
}

func NewAuthCommand() *cobra.Command {
	return &cobra.Command{
		Use:   tools.JobTypeAuth.GetSubCommand(),
		Short: "Run Two Node Fencing pcs authentication",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnfauth.RunTnfAuth()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

func NewSetupCommand() *cobra.Command {
	return &cobra.Command{
		Use:   tools.JobTypeSetup.GetSubCommand(),
		Short: "Run the Two Node Fencing setup",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnfsetup.RunTnfSetup()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

func NewAfterSetupCommand() *cobra.Command {
	return &cobra.Command{
		Use:   tools.JobTypeAfterSetup.GetSubCommand(),
		Short: "Run the Two Node Fencing after setup steps",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnfaftersetup.RunTnfAfterSetup()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

func NewFencingCommand() *cobra.Command {
	return &cobra.Command{
		Use:   tools.JobTypeFencing.GetSubCommand(),
		Short: "Run the Two Node Fencing pacemaker fencing steps",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnffencing.RunFencingSetup()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

func NewRestartEtcdCommand() *cobra.Command {
	return &cobra.Command{
		Use:   tools.JobTypeRestartEtcd.GetSubCommand(),
		Short: "Run restart etcd steps for cert change",
		Run: func(cmd *cobra.Command, args []string) {
			err := tnfrestartetcd.RunEtcdRestart()
			if err != nil {
				klog.Fatal(err)
			}
		},
	}
}

package main

import (
	goflag "flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/mount"
	operatorcmd "github.com/openshift/cluster-etcd-operator/pkg/cmd/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/staticpodcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/staticsynccontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/waitforceo"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/waitforkube"
	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/library-go/pkg/operator/staticpod/certsyncpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/installerpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/prune"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
)

func main() {
	rand.Seed(time.Now().UTC().UnixNano())

	pflag.CommandLine.SetNormalizeFunc(utilflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(goflag.CommandLine)

	logs.InitLogs()
	defer logs.FlushLogs()

	command := NewSSCSCommand()
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func NewSSCSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster-etcd-operator",
		Short: "OpenShift cluster etcd operator",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Help()
			os.Exit(1)
		},
	}

	cmd.AddCommand(operatorcmd.NewOperator())
	cmd.AddCommand(render.NewRenderCommand(os.Stderr))
	cmd.AddCommand(installerpod.NewInstaller())
	cmd.AddCommand(prune.NewPrune())
	cmd.AddCommand(certsyncpod.NewCertSyncControllerCommand(operator.CertConfigMaps, operator.CertSecrets))
	cmd.AddCommand(staticsynccontroller.NewStaticSyncCommand(os.Stderr))
	cmd.AddCommand(staticpodcontroller.NewStaticPodCommand(os.Stderr))
	cmd.AddCommand(mount.NewMountCommand(os.Stderr))
	cmd.AddCommand(waitforceo.NewWaitForCeoCommand(os.Stderr))
	cmd.AddCommand(waitforkube.NewWaitForKubeCommand(os.Stderr))

	return cmd
}

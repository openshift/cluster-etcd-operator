package main

import (
	"context"
	goflag "flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"time"

	prune_backups "github.com/openshift/cluster-etcd-operator/pkg/cmd/prune-backups"

	"github.com/openshift/cluster-etcd-operator/pkg/cmd/backuprestore"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/monitor"
	operatorcmd "github.com/openshift/cluster-etcd-operator/pkg/cmd/operator"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/readyz"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/render"
	requestbackup "github.com/openshift/cluster-etcd-operator/pkg/cmd/request-backup"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/rev"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/verify"
	"github.com/openshift/cluster-etcd-operator/pkg/cmd/waitforceo"
	"github.com/openshift/cluster-etcd-operator/pkg/operator"
	"github.com/openshift/library-go/pkg/operator/staticpod/certsyncpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/installerpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/prune"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/grpclog"
	utilflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
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

	command := NewSSCSCommand(context.Background())
	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func NewSSCSCommand(ctx context.Context) *cobra.Command {
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
	cmd.AddCommand(backuprestore.NewBackupCommand(os.Stderr))
	cmd.AddCommand(backuprestore.NewRestoreCommand(os.Stderr))
	cmd.AddCommand(installerpod.NewInstaller(ctx))
	cmd.AddCommand(prune.NewPrune())
	cmd.AddCommand(certsyncpod.NewCertSyncControllerCommand(operator.CertConfigMaps, operator.CertSecrets))
	cmd.AddCommand(waitforceo.NewWaitForCeoCommand(os.Stderr))
	cmd.AddCommand(monitor.NewMonitorCommand(os.Stderr))
	cmd.AddCommand(verify.NewVerifyCommand(os.Stderr))
	cmd.AddCommand(readyz.NewReadyzCommand())
	cmd.AddCommand(prune_backups.NewPruneCommand())
	cmd.AddCommand(requestbackup.NewRequestBackupCommand(ctx))
	cmd.AddCommand(rev.NewRevCommand(ctx))

	return cmd
}

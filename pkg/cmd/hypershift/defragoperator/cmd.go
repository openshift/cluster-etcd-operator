package defragoperator

import (
	"context"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/hypershift/defragcontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/controller/controllercmd"

	"github.com/spf13/cobra"
	"go.etcd.io/etcd/client/pkg/v3/transport"
)

var AlivenessChecker = health.NewMultiAlivenessChecker()

type defragOpts struct {
	etcdEndpoints    []string
	clientCertFile   string
	clientKeyFile    string
	clientCACertFile string
}

func NewDefragOperatorCommand() *cobra.Command {
	opts := &defragOpts{
		etcdEndpoints: []string{"https://localhost:2379"},
	}

	cmd := controllercmd.
		NewControllerCommandConfig("hypershift-etcd-defrag-operator", version.Get(), opts.runOperator).
		NewCommandWithContext(context.Background())

	cmd.Use = "etcd-defrag"
	cmd.Short = "Start the Hypershift etcd defrag Operator"

	cmd.Flags().StringSliceVar(&opts.etcdEndpoints, "etcd-endpoints", opts.etcdEndpoints, "Etcd endpoints. One endpoint is sufficient to discover all members and perform defragmentation. Default [https://localhost:2379]")
	cmd.Flags().StringVar(&opts.clientCertFile, "client-cert-file", opts.clientCertFile, "Etcd TLS client certificate file. (required)")
	cmd.Flags().StringVar(&opts.clientKeyFile, "client-key-file", opts.clientKeyFile, "Etcd TLS client key file. (required)")
	cmd.Flags().StringVar(&opts.clientCACertFile, "client-cacert-file", opts.clientCACertFile, "Etcd TLS client CA certificate file. (required)")

	cmd.MarkFlagRequired("client-cert-file")
	cmd.MarkFlagRequired("client-key-file")
	cmd.MarkFlagRequired("client-cacert-file")

	return cmd
}

func (r *defragOpts) runOperator(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {
	tlsInfo := &transport.TLSInfo{
		CertFile:      r.clientCertFile,
		KeyFile:       r.clientKeyFile,
		TrustedCAFile: r.clientCACertFile,
	}
	etcdClient := etcdcli.NewEtcdClientWithEndpointFunc(r.etcdEndpointsFunc, tlsInfo, controllerContext.EventRecorder)

	defragController := defragcontroller.NewHypershiftDefragController(AlivenessChecker, etcdClient, controllerContext.EventRecorder)

	go defragController.Run(ctx, 1)

	<-ctx.Done()
	return nil
}

func (r *defragOpts) etcdEndpointsFunc() ([]string, error) {
	return r.etcdEndpoints, nil
}

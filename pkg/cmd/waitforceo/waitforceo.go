package waitforceo

import (
	"context"
	"fmt"
	"io"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/bootstrapteardown"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

type waitForCeoOpts struct {
	errOut     io.Writer
	kubeconfig string
}

// NewWaitForCeoCommand waits for cluster-etcd-operator to bootstrap.
func NewWaitForCeoCommand(errOut io.Writer) *cobra.Command {
	waitForCeoOpts := &waitForCeoOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "wait-for-ceo --FLAGS",
		Short: "waits for cluster-etcd-operator to finish bootstrap etcd cluster",
		Long:  "This command makes sure that the cluster-etcd-operator is available before signaling bootstrap is done",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
						fmt.Fprint(waitForCeoOpts.errOut, err.Error())
					}
				}
			}
			must(waitForCeoOpts.Run)
		},
	}

	waitForCeoOpts.AddFlags(cmd.Flags())
	return cmd
}

func (w *waitForCeoOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&w.kubeconfig, "kubeconfig", "", "kubeconfig for the cluster it is bootstrapping")
}

func (w *waitForCeoOpts) Run() error {
	config, err := clientcmd.BuildConfigFromFlags("", w.kubeconfig)
	if err != nil {
		klog.Errorf("error loading kubeconfig: %#v", err)
	}
	if err := bootstrapteardown.WaitForEtcdBootstrap(context.TODO(), config); err != nil {
		klog.Errorf("error waiting for cluster-etcd-operator %#v", err)
	}
	return nil
}

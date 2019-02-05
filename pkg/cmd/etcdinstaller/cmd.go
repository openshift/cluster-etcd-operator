package etcdinstaller

import (
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	"github.com/openshift/library-go/pkg/operator/staticpod/installerpod"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
)

type InstallOptions struct {
	InstallOptions installerpod.InstallOptions

	PodArgs []string
}

func NewInstallOptions() *InstallOptions {
	options := installerpod.NewInstallOptions()
	ret := &InstallOptions{
		InstallOptions: *options,
	}
	ret.InstallOptions.WithPodMutationFn(ret.PodMutator)

	return ret
}

func (o *InstallOptions) PodMutator(pod *corev1.Pod) error {
	pod.Spec.Containers[0].Args = o.PodArgs
	return nil
}

// NewInstaller is a custom command for installing a pod that has a mutation function for manipulating the pod
// based on which node is being installed to
func NewInstaller() *cobra.Command {
	o := NewInstallOptions()

	cmd := &cobra.Command{
		Use:   "installer",
		Short: "Install static pod and related resources",
		Run: func(cmd *cobra.Command, args []string) {
			glog.V(1).Info(cmd.Flags())
			glog.V(1).Info(spew.Sdump(o))

			if err := o.InstallOptions.Complete(); err != nil {
				glog.Fatal(err)
			}
			if err := o.InstallOptions.Validate(); err != nil {
				glog.Fatal(err)
			}
			if err := o.InstallOptions.Run(); err != nil {
				glog.Fatal(err)
			}
		},
	}

	o.AddFlags(cmd.Flags())

	return cmd
}

func (o *InstallOptions) AddFlags(fs *pflag.FlagSet) {
	o.InstallOptions.AddFlags(fs)
	fs.StringSliceVar(&o.PodArgs, "pod-args", o.PodArgs, "args to be set on the container")
}

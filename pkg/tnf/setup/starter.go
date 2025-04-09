package setup

import (
	"context"
	"os"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/setup/controller"
)

func RunTnfSetup(ctx context.Context, controllerContext *controllercmd.ControllerContext) error {

	// This kube client use protobuf, do not use it for CR
	kubeClient, err := kubernetes.NewForConfig(controllerContext.ProtoKubeConfig)
	if err != nil {
		return err
	}

	operatorConfigClient, err := operatorversionedclient.NewForConfig(controllerContext.KubeConfig)
	if err != nil {
		return err
	}

	tnfSetupRunner := controller.NewTnfSetupRunner(
		kubeClient,
		operatorConfigClient,
		controllerContext.EventRecorder,
		os.Getenv("ETCD_IMAGE_PULLSPEC"),
	)

	err = tnfSetupRunner.Run(ctx)
	if err != nil {
		klog.Errorf("Error running setup: %v", err)
	}

	<-ctx.Done()

	return err
}

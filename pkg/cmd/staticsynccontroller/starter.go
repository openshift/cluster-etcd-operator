package staticsynccontroller

import (
	"context"
	"fmt"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func RunController() error {
	ctx, cancel := context.WithCancel(context.Background())
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	kubeInformerFactory := informers.NewFilteredSharedInformerFactory(clientset, 0, "openshift-etcd", nil)

	staticSyncController := NewStaticSyncController(
		kubeInformerFactory,
		nil,
	)

	kubeInformerFactory.Start(ctx.Done())

	go staticSyncController.Run(ctx.Done())

	<-ctx.Done()
	cancel()
	return fmt.Errorf("stopped")
}

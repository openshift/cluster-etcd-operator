package bootstrapteardown

import (
	"context"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	clientwatch "k8s.io/client-go/tools/watch"
	"k8s.io/klog/v2"
)

func WaitForEtcdBootstrap(ctx context.Context, config *rest.Config) error {
	operatorConfigClient, err := operatorversionedclient.NewForConfig(config)
	if err != nil {
		klog.Errorf("error getting operator client config: %#v", err)
		return err
	}

	// TODO: figure out if we can timeout after 30 mins by
	// passing a different context here
	if err := waitForEtcdBootstrap(ctx, operatorConfigClient.OperatorV1().RESTClient()); err != nil {
		klog.Errorf("error watching etcd: %#v", err)
	}
	return err
}

func waitForEtcdBootstrap(ctx context.Context, operatorRestClient rest.Interface) error {
	_, err := clientwatch.UntilWithSync(
		ctx,
		cache.NewListWatchFromClient(operatorRestClient, "etcds", "", fields.OneTermEqualSelector("metadata.name", "cluster")),
		&operatorv1.Etcd{},
		nil,
		func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Added, watch.Modified:
				etcd, ok := event.Object.(*operatorv1.Etcd)
				if !ok {
					klog.Warningf("Expected an Etcd object but got a %q object instead", event.Object.GetObjectKind().GroupVersionKind())
					return false, nil
				}
				return done(etcd)
			}
			klog.Infof("Still waiting for the cluster-etcd-operator to bootstrap...")
			return false, nil
		},
	)

	if err != nil {
		klog.Errorf("error waiting for etcd CR: %#v", err)
		return err
	}

	klog.Infof("cluster-etcd-operator bootstrap etcd")
	return nil
}

func done(etcd *operatorv1.Etcd) (bool, error) {
	if operatorv1helpers.IsOperatorConditionTrue(etcd.Status.Conditions, "EtcdRunningInCluster") {
		klog.Info("Cluster etcd operator bootstrapped successfully")
		return true, nil
	}
	klog.Infof("waiting on condition %s in etcd CR %s/%s to be True.", "EtcdRunningInCluster", etcd.Namespace, etcd.Name)
	return false, nil
}

func WaitForEtcdBootstrapRemoval(ctx context.Context, config *rest.Config) error {
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("error getting kube client: %#v", err)
		return err
	}

	_, err = clientwatch.UntilWithSync(
		ctx,
		cache.NewListWatchFromClient(kubeClient.RESTClient(), "bootstrap", "kube-system", fields.OneTermEqualSelector("metadata.name", "bootstrap")),
		&corev1.ConfigMap{},
		nil,
		func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Modified:
				cm, ok := event.Object.(*corev1.ConfigMap)
				if !ok {
					klog.Warningf("Expected configmap object but got a %q object instead", event.Object.GetObjectKind().GroupVersionKind())
					return false, nil
				}

				if cm.Data["etcd-bootstrap-removal-status"] == "complete" {
					klog.Infof("cluster-etcd-operator successfully removed the bootstrap member")
					return true, nil
				}
			}
			klog.Infof("Still waiting for the cluster-etcd-operator to remove bootstrap member...")
			return false, nil
		},
	)

	if err != nil {
		klog.Errorf("error waiting for etcd bootstrap member removal: %#v", err)
		return err
	}

	klog.Infof("cluster-etcd-operator has removed the bootstrap etcd member")
	return nil
}

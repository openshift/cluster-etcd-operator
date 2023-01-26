package bootstrapteardown

import (
	"context"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
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
				return done(etcd), nil
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

func done(etcd *operatorv1.Etcd) bool {
	scalingStrategy := operatorv1helpers.FindOperatorCondition(etcd.Status.Conditions, "BootstrapScalingStrategy")
	if scalingStrategy != nil {
		// for DelayedHAScalingStrategy we need to test whether the bootstrap member was already removed
		// this is to ensure we properly remove the bootstrap member before it comes back as a proper control plane node
		if scalingStrategy.Message == string(ceohelpers.DelayedHAScalingStrategy) {
			if operatorv1helpers.IsOperatorConditionTrue(etcd.Status.Conditions, "EtcdBoostrapMemberRemoved") {
				return true
			}

			klog.Infof("waiting on condition %s in etcd CR %s/%s to be True.", "EtcdBoostrapMemberRemoved", etcd.Namespace, etcd.Name)
			return false
		}

		if operatorv1helpers.IsOperatorConditionTrue(etcd.Status.Conditions, "EtcdRunningInCluster") {
			klog.Info("Cluster etcd operator bootstrapped successfully")
			return true
		} else {
			klog.Infof("waiting on condition %s in etcd CR %s/%s to be True.", "EtcdRunningInCluster", etcd.Namespace, etcd.Name)
		}
	} else {
		klog.Infof("waiting on condition %s in etcd CR %s/%s to be appear.", "BootstrapScalingStrategy", etcd.Namespace, etcd.Name)
	}
	return false
}

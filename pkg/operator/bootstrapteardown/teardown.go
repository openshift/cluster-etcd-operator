package bootstrapteardown

import (
	"context"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	etcdv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

func TearDownBootstrap(config *rest.Config,
	clusterMemberShipController *clustermembercontroller.ClusterMemberController, etcdClient etcdv1.EtcdInterface) error {
	err := WaitForEtcdBootstrap(context.TODO(), config, TearDownTimeout)
	if err != nil {
		klog.Errorf("WaitForEtcdBootstrap failed with: %#v", err)
	}

	err = wait.PollInfinite(5*time.Second, func() (bool, error) {
		etcd, err := etcdClient.Get("cluster", metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		klog.Infof("clusterversions is available, safe to remove bootstrap")
		switch etcd.Spec.ManagementState {
		case operatorv1.Managed:
			if clusterMemberShipController.IsMember("etcd-bootstrap") {
				//TODO: need to keep retry as long as we are sure bootstrap is not removed
				if err := clusterMemberShipController.RemoveBootstrap(); err != nil {
					klog.Errorf("error removing bootstrap %#v\n", err)
					return false, nil
				}
				return true, nil
			}
			return true, nil
		case operatorv1.Unmanaged:
			return true, nil
			// TODO handle default
		}
		// TODO: how to handle this?
		return true, nil
	})

	return err
}

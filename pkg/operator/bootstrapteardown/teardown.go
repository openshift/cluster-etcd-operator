package bootstrapteardown

import (
	"context"
	"k8s.io/apimachinery/pkg/util/wait"
	"time"

	etcdv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/rest"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	cov1helpers "github.com/openshift/library-go/pkg/config/clusteroperator/v1helpers"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	clientwatch "k8s.io/client-go/tools/watch"
	"k8s.io/klog"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
)

func TearDownBootstrap(config *rest.Config,
	clusterMemberShipController *clustermembercontroller.ClusterMemberController, etcdClient etcdv1.EtcdInterface) error {
	failing := configv1.ClusterStatusConditionType("Failing")
	var lastError string

	cc := configclient.NewForConfigOrDie(config)
	_, _ = clientwatch.UntilWithSync(
		context.Background(),
		cache.NewListWatchFromClient(cc.ConfigV1().RESTClient(), "clusterversions", "", fields.OneTermEqualSelector("metadata.name", "version")),
		&configv1.ClusterVersion{},
		nil,
		func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Added, watch.Modified:
				cv, ok := event.Object.(*configv1.ClusterVersion)
				if !ok {
					klog.Warningf("Expected a ClusterVersion object but got a %q object instead", event.Object.GetObjectKind().GroupVersionKind())
					return false, nil
				}
				if cov1helpers.IsStatusConditionTrue(cv.Status.Conditions, configv1.OperatorAvailable) {
					return true, nil
				}
				if cov1helpers.IsStatusConditionTrue(cv.Status.Conditions, failing) {
					lastError = cov1helpers.FindStatusCondition(cv.Status.Conditions, failing).Message
				} else if cov1helpers.IsStatusConditionTrue(cv.Status.Conditions, configv1.OperatorProgressing) {
					lastError = cov1helpers.FindStatusCondition(cv.Status.Conditions, configv1.OperatorProgressing).Message
				}
				klog.Errorf("Still waiting for the cluster to initialize: %s", lastError)
				return false, nil
			}
			klog.Infof("Still waiting for the cluster to initialize...")
			return false, nil
		},
	)

	err := wait.PollInfinite(5*time.Second, func() (bool, error) {
		etcd, err := etcdClient.Get("cluster", metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		klog.Infof("clusterversions is available, safe to remove bootstrap")
		switch etcd.Spec.ManagementState {
		case operatorv1.Managed:
			if clusterMemberShipController.IsMember("etcd-bootstrap") {
				//TODO: need to keep retry as long as we are sure bootstrap is not removed
				err := clusterMemberShipController.RemoveBootstrap()
				if err != nil {
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

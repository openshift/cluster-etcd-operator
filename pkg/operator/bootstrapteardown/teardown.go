package bootstrapteardown

import (
	"context"

	"k8s.io/client-go/rest"

	configv1 "github.com/openshift/api/config/v1"
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
	clusterMemberShipController *clustermembercontroller.ClusterMemberController) error {
	failing := configv1.ClusterStatusConditionType("Failing")
	var lastError string
	var err error

	cc := configclient.NewForConfigOrDie(config)
	_, err = clientwatch.UntilWithSync(
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

	if err == nil {
		klog.Infof("clusterversions is available, safe to remove bootstrap")
		if clusterMemberShipController.IsMember("etcd-bootstrap") {
			return clusterMemberShipController.RemoveBootstrap()
		}
		return nil
	}

	return nil
}

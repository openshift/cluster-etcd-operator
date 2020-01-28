package bootstrapteardown

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	clientwatch "k8s.io/client-go/tools/watch"
	"k8s.io/klog"
)

const configMapName = "config"
const configMapKey = "config.yaml"

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
				return doneEtcd(etcd)
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

func doneEtcd(etcd *operatorv1.Etcd) (bool, error) {
	if etcd.Spec.ManagementState == operatorv1.Unmanaged {
		klog.Info("Cluster etcd operator is in Unmanaged mode")
		return true, nil
	}
	if operatorv1helpers.IsOperatorConditionTrue(etcd.Status.Conditions, operatorv1.OperatorStatusTypeAvailable) &&
		operatorv1helpers.IsOperatorConditionFalse(etcd.Status.Conditions, operatorv1.OperatorStatusTypeProgressing) &&
		operatorv1helpers.IsOperatorConditionFalse(etcd.Status.Conditions, operatorv1.OperatorStatusTypeDegraded) {
		klog.Info("Cluster etcd operator bootstrapped successfully")
		return true, nil
	}
	klog.Infof("Still waiting for the cluster-etcd-operator to bootstrap")
	return false, nil
}

func WaitForKubeApiServerRollout(ctx context.Context, config *rest.Config) error {
	operatorClient, err := operatorversionedclient.NewForConfig(config)
	if err != nil {
		klog.Errorf("WaitForKubeApiServerRollout: error getting operator client: %#v", err)
		return err
	}
	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("WaitForKubeApiServerRollout: error getting kube client: %#v", err)
		return err
	}

	// TODO: figure out if we can timeout after 30 mins by
	// passing a different context here
	if err := waitForKASOperator(ctx, operatorClient.OperatorV1().RESTClient(), coreClient.CoreV1()); err != nil {
		klog.Errorf("WaitForKubeApiServerRollout: error watching kubeapiservers: %#v", err)
	}
	return err
}

func waitForKASOperator(ctx context.Context, operatorConfig rest.Interface, configMapsGetter v1.ConfigMapsGetter) interface{} {
	_, err := clientwatch.UntilWithSync(
		ctx,
		cache.NewListWatchFromClient(operatorConfig, "kubeapiservers", "", fields.OneTermEqualSelector("metadata.name", "cluster")),
		&operatorv1.KubeAPIServer{},
		nil,
		func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Added, watch.Modified:
				apiserver, ok := event.Object.(*operatorv1.KubeAPIServer)
				if !ok {
					klog.Warningf("Expected a KubeAPIServer object but got a %q object instead", event.Object.GetObjectKind().GroupVersionKind())
					return false, nil
				}
				return doneApiServer(apiserver, configMapsGetter), nil
			}
			klog.Infof("Still waiting for kube-apiserver operator to be healthy...")
			return false, nil
		},
	)

	if err != nil {
		klog.Errorf("waitForKASOperator: error waiting for kube-apiserver operator: %#v", err)
		return err
	}

	klog.Infof("kube-apiserver operator is healthy")
	return nil
}

func doneApiServer(kasOperator *operatorv1.KubeAPIServer, configMapsGetter v1.ConfigMapsGetter) bool {
	revisionMap := map[int32]struct{}{}
	uniqueRevisions := []int32{}

	for _, nodeStatus := range kasOperator.Status.NodeStatuses {
		revision := nodeStatus.CurrentRevision
		if _, ok := revisionMap[revision]; !ok {
			revisionMap[revision] = struct{}{}
			uniqueRevisions = append(uniqueRevisions, revision)
		}
	}

	// For each revision, check that the configmap for that revision contains the
	// appropriate storageConfig
	done := false
	for _, revision := range uniqueRevisions {
		configMapNameWithRevision := fmt.Sprintf("%s-%d", configMapName, revision)
		configMap, err := configMapsGetter.ConfigMaps("openshift-kube-apiserver").Get(configMapNameWithRevision, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("doneApiServer: error getting configmap: %#v", err)
			return false
		}
		if configMapHasRequiredValues(configMap) {
			// if any 1 kube-apiserver pod has more than 1
			done = true
			break
		}
	}
	return done
}

type ConfigData struct {
	StorageConfig struct {
		Urls []string
	}
}

func configMapHasRequiredValues(configMap *corev1.ConfigMap) bool {
	config, ok := configMap.Data[configMapKey]
	if !ok {
		klog.Errorf("configMapHasRequiredValues: config.yaml key missing")
		return false
	}
	var configData ConfigData
	err := json.Unmarshal([]byte(config), &configData)
	if err != nil {
		klog.Errorf("configMapHasRequiredValues: error unmarshalling configmap data : %#v", err)
		return false
	}
	if len(configData.StorageConfig.Urls) == 0 {
		klog.Infof("configMapHasRequiredValues: length of storageUrls %#v is 0", configData.StorageConfig.Urls)
		return false
	}
	if len(configData.StorageConfig.Urls) == 1 &&
		!strings.Contains(configData.StorageConfig.Urls[0], "etcd") {
		klog.Infof("configMapHasRequiredValues: config just has bootstrap IP: %#v", configData.StorageConfig.Urls)
		return false
	}
	return true
}

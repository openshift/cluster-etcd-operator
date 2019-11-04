package clustermembercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1client "k8s.io/client-go/kubernetes"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
)

const (
	workQueueKey             = "key"
	EtcdScalingAnnotationKey = "etcd.operator.openshift.io/scale"
	etcdCertFile             = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile              = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile        = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
	EtcdEndpointNamespace    = "openshift-etcd"
	EtcdHostEndpointName     = "host-etcd"
	EtcdEndpointName         = "etcd"
)

type ClusterMemberController struct {
	clientset                              corev1client.Interface
	operatorConfigClient                   v1helpers.OperatorClient
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory
	queue                                  workqueue.RateLimitingInterface
	eventRecorder                          events.Recorder
	etcdDiscoveryDomain                    string
}

func NewClusterMemberController(
	clientset corev1client.Interface,
	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
	etcdDiscoveryDomain string,
) *ClusterMemberController {
	c := &ClusterMemberController{
		clientset:                              clientset,
		operatorConfigClient:                   operatorConfigClient,
		queue:                                  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController"),
		kubeInformersForOpenshiftEtcdNamespace: kubeInformersForOpenshiftEtcdNamespace,
		eventRecorder:                          eventRecorder.WithComponentSuffix("cluster-member-controller"),
		etcdDiscoveryDomain:                    etcdDiscoveryDomain,
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ClusterMemberController) sync() error {
	operatorSpec, _, _, err := c.operatorConfigClient.GetOperatorState()
	if err != nil {
		return err
	}
	switch operatorSpec.ManagementState {
	case operatorv1.Managed:
	case operatorv1.Unmanaged:
		return nil
	case operatorv1.Removed:
		// TODO should we support removal?
		return nil
	default:
		c.eventRecorder.Warningf("ManagementStateUnknown", "Unrecognized operator management state %q", operatorSpec.ManagementState)
		return nil
	}

	pods, err := c.clientset.CoreV1().Pods("openshift-etcd").List(metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	if err != nil {
		klog.Infof("No Pod found in openshift-etcd with label k8s-app=etcd")
		return err
	}

	resyncName, err := c.getResyncName(pods)
	for i := range pods.Items {
		p := &pods.Items[i]
		klog.Infof("Found etcd Pod with name %v\n", p.Name)

		// we anchor this loop on the configmap. In the case of failure we can resync by aligning with that Pod
		switch resyncName {
		case "":
			break
		case p.Name:
			klog.Infof("resyncing on %s\n", p.Name)
		default:
			continue
		}

		// exisiting member can be removed order is important here
		if c.IsStatus("pending", p.Name, ceoapi.MemberRemove) {
			klog.Infof("Member is unhealthy and is being removed: %s\n", p.Name)
			if err := c.etcdMemberRemove(p.Name); err != nil {
				c.eventRecorder.Warning("ScalingDownFailed", err.Error())
				return err
				// Todo alaypatel07:  need to take care of condition degraded
				// Todo alaypatel07: need to skip this reconciliation loop and continue later
				// after the member is removed from this very point.
			}
			// continue?
		}

		if c.IsMember(p.Name) {
			klog.Infof("Member is already part of the cluster %s\n", p.Name)
			name, err := c.getScaleAnnotationName()
			if err != nil {
				klog.Errorf("failed to obtain name from annotation %v", err)
			}
			// clear annotation because scaling is complete
			if name == p.Name {
				if err := c.setScaleAnnotation(""); err != nil {
					return err
				}
			}
			continue
		}

		condUpgradable := operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeUpgradeable,
			Status: operatorv1.ConditionFalse,
		}
		condProgressing := operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeProgressing,
			Status: operatorv1.ConditionTrue,
		}
		// Setting the available false when scaling. This will prevent installer from reporting
		// success when any of the members are not ready
		condAvailable := operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeAvailable,
			Status: operatorv1.ConditionFalse,
		}
		if _, _, updateError := v1helpers.UpdateStatus(c.operatorConfigClient,
			v1helpers.UpdateConditionFn(condUpgradable),
			v1helpers.UpdateConditionFn(condProgressing),
			v1helpers.UpdateConditionFn(condAvailable)); updateError != nil {
			return updateError
		}

		// although we dont use SRV for server bootstrap we do use the records to map peerurls
		peerFQDN, err := ReverseLookupSelf("etcd-server-ssl", "tcp", c.etcdDiscoveryDomain, p.Status.HostIP)
		if err != nil {
			klog.Errorf("error looking up self: %v", err)
			continue
		}

		// Pending MemberReady: etcd is free join cluster here we provide configurations nessisary to fullfill dependencies.
		// if c.IsStatus("pending", p.Name, ceoapi.MemberReady) {
		members, err := c.EtcdList("members")
		if err != nil {
			return err
		}

		es := ceoapi.EtcdScaling{
			Metadata: &metav1.ObjectMeta{
				Name:              p.Name,
				CreationTimestamp: metav1.Time{Time: time.Now()},
			},
			Members: members,
			PodFQDN: peerFQDN,
		}

		esb, err := json.Marshal(es)
		if err != nil {
			return err
		}

		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.setScaleAnnotation(string(esb)); err != nil {
				return err
			}
			return nil
		})
		if retryErr != nil {
			return fmt.Errorf("Update approve failed: %v", retryErr)
		}
		// }

		// Pending MemberAdd: here we have observed the static pod having add dependencies filled ok to scale Cluster API.
		if c.IsStatus("pending", p.Name, ceoapi.MemberReady) {
			if err := c.etcdMemberAdd([]string{fmt.Sprintf("https://%s:2380", peerFQDN)}); err != nil {
				c.eventRecorder.Warning("ScalingFailed", err.Error())
				return err
			}
		}
		if c.IsMember(p.Name) {
			klog.Infof("Member is already part of the cluster: %s\n", p.Name)
			continue
		}

		// should not happen
		rerr := fmt.Errorf("failed scale member %s", p.Name)
		c.eventRecorder.Warning("ScalingFailed", rerr.Error())
		return rerr
	}
	klog.Infof("All cluster members observed, scaling complete!")
	// report available
	condAvailable := operatorv1.OperatorCondition{
		Type:   operatorv1.OperatorStatusTypeAvailable,
		Status: operatorv1.ConditionTrue,
	}
	condUpgradable := operatorv1.OperatorCondition{
		Type:   operatorv1.OperatorStatusTypeUpgradeable,
		Status: operatorv1.ConditionTrue,
	}
	condProgressing := operatorv1.OperatorCondition{
		Type:   operatorv1.OperatorStatusTypeProgressing,
		Status: operatorv1.ConditionFalse,
	}

	if _, _, updateError := v1helpers.UpdateStatus(c.operatorConfigClient,
		v1helpers.UpdateConditionFn(condAvailable),
		v1helpers.UpdateConditionFn(condUpgradable),
		v1helpers.UpdateConditionFn(condProgressing)); updateError != nil {
		klog.Infof("Error updating status %#v", err)
		return updateError
	}

	return nil
}

func (c *ClusterMemberController) Endpoints() ([]string, error) {
	storageConfigURLsPath := []string{"storageConfig", "urls"}
	operatorSpec, _, _, err := c.operatorConfigClient.GetOperatorState()
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(operatorSpec.ObservedConfig.Raw)).Decode(&config); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}
	endpoints, exists, err := unstructured.NestedStringSlice(config, storageConfigURLsPath...)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("etcd storageConfig urls not observed")
	}

	return []string{endpoints[0]}, nil
}

func (c *ClusterMemberController) getEtcdClient() (*clientv3.Client, error) {
	endpoints, err := c.Endpoints()
	if err != nil {
		return nil, err
	}
	tlsInfo := transport.TLSInfo{
		CertFile:      etcdCertFile,
		KeyFile:       etcdKeyFile,
		TrustedCAFile: etcdTrustedCAFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()

	cfg := &clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return cli, err
}

func (c *ClusterMemberController) etcdMemberRemove(name string) error {
	cli, err := c.getEtcdClient()
	if err != nil {
		return err
	}
	defer cli.Close()
	l, err := cli.MemberList(context.Background())
	if err != nil {
		return err
	}
	for _, member := range l.Members {
		if member.Name == name {

			resp, err := cli.MemberRemove(context.Background(), member.ID)
			if err != nil {
				return err
			}
			klog.Infof("Members left %#v", resp.Members)
		}
	}
	return nil
}

func (c *ClusterMemberController) EtcdList(bucket string) ([]ceoapi.Member, error) {
	configPath := []string{"cluster", bucket}
	operatorSpec, _, _, err := c.operatorConfigClient.GetOperatorState()
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(operatorSpec.ObservedConfig.Raw)).Decode(&config); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}
	data, exists, err := unstructured.NestedSlice(config, configPath...)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("etcd cluster members not observed")
	}

	// populate current etcd members as observed.
	var members []ceoapi.Member
	for _, member := range data {
		memberMap, _ := member.(map[string]interface{})
		name, exists, err := unstructured.NestedString(memberMap, "name")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member name does not exist")
		}
		peerURLs, exists, err := unstructured.NestedString(memberMap, "peerURLs")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member peerURLs do not exist")
		}
		// why have different terms i.e. status and condition? can we choose one and mirror?
		status, exists, err := unstructured.NestedString(memberMap, "status")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member status does not exist")
		}

		condition := ceoapi.GetMemberCondition(status)
		m := ceoapi.Member{
			Name:     name,
			PeerURLS: []string{peerURLs},
			Conditions: []ceoapi.MemberCondition{
				{
					Type: condition,
				},
			},
		}
		members = append(members, m)
	}
	return members, nil
}

func (c *ClusterMemberController) IsMember(name string) bool {
	members, _ := c.EtcdList("members")
	for _, m := range members {
		if m.Name == name {
			return true
		}
	}
	return false
}

// IsStatus returns true or false based on the bucket name and status of an etcd. If multiple status are passed
// the compare is done using or so if true one of the status exists for that etcd.
func (c *ClusterMemberController) IsStatus(bucket string, name string, condition ...ceoapi.MemberConditionType) bool {
	members, _ := c.EtcdList(bucket)
	for _, m := range members {
		klog.Warningf("IsMemberRemove: checking %v vs %v type = %v\n", m.Name, name, m.Conditions[0].Type)
		if m.Name == name {
			for _, status := range condition {
				if m.Conditions[0].Type == status {
					return true
				}
			}
		}
	}
	return false
}

func (c *ClusterMemberController) setScaleAnnotation(scaling string) error {
	result, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if result.Annotations == nil {
		result.Annotations = make(map[string]string)
	}
	result.Annotations[EtcdScalingAnnotationKey] = scaling
	_, updateErr := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Update(result)
	if updateErr != nil {
		return updateErr
	}

	return nil
}

func (c *ClusterMemberController) getScaleAnnotationName() (string, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return GetScaleAnnotationName(cm)
}

func (c *ClusterMemberController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController")
	defer klog.Infof("Shutting down ClusterMemberController")

	if !cache.WaitForCacheSync(stopCh,
		c.kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().HasSynced,
		c.kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Informer().HasSynced) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *ClusterMemberController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *ClusterMemberController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *ClusterMemberController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

func (c *ClusterMemberController) etcdMemberAdd(peerURLs []string) error {
	cli, err := c.getEtcdClient()
	if err != nil {
		return err
	}
	defer cli.Close()
	resp, err := cli.MemberAdd(context.Background(), peerURLs)
	if err != nil {
		return err
	}
	klog.Infof("added etcd member.PeerURLs:%s", resp.Member.PeerURLs)
	return nil
}

func (c *ClusterMemberController) RemoveBootstrap() error {
	err := c.RemoveBootstrapFromEndpoint()
	if err != nil {
		return err
	}
	return c.etcdMemberRemove("etcd-bootstrap")
}

func (c *ClusterMemberController) RemoveBootstrapFromEndpoint() error {
	hostEndpoint, err := c.clientset.CoreV1().
		Endpoints(EtcdEndpointNamespace).
		Get(EtcdHostEndpointName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting endpoint: %#v\n", err)
		return err
	}
	subsetIndex := -1
	bootstrapIndex := -1
	for sI, s := range hostEndpoint.Subsets {
		for i, s := range s.Addresses {
			if s.Hostname == "etcd-bootstrap" {
				bootstrapIndex = i
				subsetIndex = sI
				break
			}
		}
	}

	if subsetIndex == -1 || bootstrapIndex == -1 {
		// Unable to find bootstrap
		return nil
	}

	hostEndpoint.Subsets[subsetIndex].Addresses = append(hostEndpoint.Subsets[subsetIndex].Addresses[0:bootstrapIndex], hostEndpoint.Subsets[subsetIndex].Addresses[bootstrapIndex+1:]...)

	_, err = c.clientset.CoreV1().Endpoints(EtcdEndpointNamespace).Update(hostEndpoint)
	if err != nil {
		klog.Errorf("error updating endpoint: %#v\n", err)
		return err
	}
	return nil
}

func (c *ClusterMemberController) getResyncName(pods *corev1.PodList) (string, error) {
	name, err := c.getScaleAnnotationName()
	if err != nil {
		return "", fmt.Errorf("failed to obtain name from annotation %v", err)
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		klog.Errorf("getResyncName: compare %s vs %s\n", p.Name, name)
		if p.Name == name {
			return name, nil
		}
	}
	return "", nil
}

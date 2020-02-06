package clustermembercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey                   = "key"
	EtcdScalingAnnotationKey       = "etcd.operator.openshift.io/scale"
	etcdCertFile                   = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile                    = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile              = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
	EtcdEndpointNamespace          = "openshift-etcd"
	EtcdHostEndpointName           = "host-etcd"
	EtcdEndpointName               = "etcd"
	ConditionBootstrapSafeToRemove = "BootstrapSafeToRemove"
	ConditionBootstrapRemoved      = "BootstrapRemoved"
	waitForKubeContainerNumber     = 0
	EtcdClusterSize                = 3
)

type ClusterMemberController struct {
	clientset                              corev1client.Interface
	operatorConfigClient                   v1helpers.OperatorClient
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory
	queue                                  workqueue.RateLimitingInterface
	eventRecorder                          events.Recorder
	etcdDiscoveryDomain                    string
	numberOfEtcdMembers                    int
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
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	operatorConfigClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ClusterMemberController) sync() error {
	err := c.reconcileMembers()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "ClusterMemberDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *ClusterMemberController) reconcileMembers() error {
	err := c.setNumberOfEtcdMembers()
	if err != nil {
		return err
	}

	memberForScaleDown, err := c.getNewMemberToScaleDown()
	if err != nil {
		return err
	}

	if memberForScaleDown != nil {
		// emit event, update status
		c.eventRecorder.Warningf("EtcdMemberScaleDown", "Member is unhealthy and is being removed: %s\n", memberForScaleDown.Name)
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberRemoveProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdMemberRemove",
			Message: fmt.Sprintf("Removing member %s", memberForScaleDown.Name),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
		if err := c.EtcdMemberRemove(memberForScaleDown.Name); err != nil {
			c.eventRecorder.Warning("ScalingDownFailed", err.Error())
			return err
		}
		return nil
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "ClusterMemberRemoveProgressing",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
	}

	// for scaling up, check if configmap is populated
	resyncName, err := c.getScaleAnnotationName()
	if err != nil {
		return fmt.Errorf("failed to obtain name from annotation: %s", err.Error())
	}

	if resyncName == "" {
		c.eventRecorder.Eventf("ScaleUpAnnotationEmpty", "no annotation found on the configmap")
		newMemberForScaleUp, err := c.getNewMemberToScaleUp()
		if err != nil {
			return err
		}
		if newMemberForScaleUp == nil {
			if c.numberOfEtcdMembers == EtcdClusterSize {
				klog.V(4).Infof("reconcileMembers: scaling complete")
				_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient,
					v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
						Type:    "ClusterMemberProgressing",
						Status:  operatorv1.ConditionFalse,
						Reason:  "ClusterMemberScalingComplete",
						Message: fmt.Sprintf("etcd has scaled to %d/%d members", c.numberOfEtcdMembers, EtcdClusterSize),
					}),
					v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
						Type:    ConditionBootstrapSafeToRemove,
						Status:  operatorv1.ConditionTrue,
						Reason:  "ScalingComplete",
						Message: fmt.Sprintf("etcd has scaled to %d masters", c.numberOfEtcdMembers),
					}))
				if updateErr != nil {
					c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
					return updateErr
				}
			} else {
				// we should never hit this else, but if we do, we should know that state
				c.eventRecorder.Warningf("NoMemberToScaleUp", "No new member found for scaling up")
				_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
					Type:    "ClusterMemberProgressing",
					Status:  operatorv1.ConditionTrue,
					Reason:  "ClusterMemberScalingIncomplete",
					Message: fmt.Sprintf("etcd is scaling with %d/%d members active", c.numberOfEtcdMembers, EtcdClusterSize)}),
					v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
						Type:    ConditionBootstrapSafeToRemove,
						Status:  operatorv1.ConditionFalse,
						Reason:  "ScalingIncomplete",
						Message: "etcd is still scaling",
					}))
				if updateErr != nil {
					c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
					return updateErr
				}
			}
		}

		err = c.setScaleAnnotation(newMemberForScaleUp)
		if err != nil {
			return err
		}
		// just changed the configmap, we will sync on it during next run
		return nil
	}

	podToResync, err := c.clientset.CoreV1().Pods("openshift-etcd").Get(resyncName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	klog.V(4).Infof("reconciling %s/%s", podToResync.Namespace, podToResync.Name)

	if c.IsMember(podToResync.Name) {
		klog.Infof("Member is already part of the cluster %s\n", podToResync.Name)
		// clear annotation because scaling is complete
		if err := c.setScaleAnnotation(nil); err != nil {
			return err
		}
		c.eventRecorder.Eventf("ScaleUpAnnotationCleared", "Member %s has scaled", resyncName)
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberAvailable",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdMemberAvailable",
			Message: fmt.Sprintf("scaling member %s is successful, it is now available", podToResync.Name),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberAvailable",
			Status:  operatorv1.ConditionFalse,
			Reason:  "ScaleUpProgressing",
			Message: fmt.Sprintf("Scaling member %s, it is not available yet", podToResync.Name),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
	}

	if c.IsStatus("pending", podToResync.Name, ceoapi.MemberReady) {
		scalingData, err := c.GetScalingDataFromConfigMap()
		if err != nil {
			return err
		}
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberAddProgressing",
			Status:  operatorv1.ConditionTrue,
			Reason:  "EtcdMemberAdd",
			Message: fmt.Sprintf("Adding member %s to etcd", podToResync.Name),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
		c.eventRecorder.Eventf("ScalingMember", "Adding member %s with scaling data %v", podToResync.Name, scalingData)
		if err := c.etcdMemberAdd([]string{fmt.Sprintf("https://%s:2380", scalingData.PodFQDN)}); err != nil {
			c.eventRecorder.Warning("ScalingFailed", err.Error())
			return err
		}
	} else {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorConfigClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "ClusterMemberAddProgressing",
			Status:  operatorv1.ConditionFalse,
			Reason:  "EtcdMemberAdded",
			Message: fmt.Sprintf("Member %s added to etcd", podToResync.Name),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("ClusterMemberErrorUpdatingStatus", updateErr.Error())
			return updateErr
		}
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

	klog.V(2).Infof("Endpoints: creating etcd client with endpoints %s", strings.Join(endpoints, ", "))
	return endpoints, nil
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

func (c *ClusterMemberController) EtcdMemberRemove(name string) error {
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
			c.eventRecorder.Eventf("EtcdMemberRemoved", "removed %s from etcd api, members left %#v", name, resp.Members)
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
	// populate current etcd members as observed.
	members := []ceoapi.Member{}
	if !exists {
		klog.Infof("bucket %s empty", bucket)
		return members, nil
	}

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

func (c *ClusterMemberController) IsEtcdMember(name string) bool {
	cli, err := c.getEtcdClient()
	if err != nil {
		return false
	}
	defer cli.Close()
	l, err := cli.MemberList(context.Background())
	if err != nil {
		return false
	}
	for _, m := range l.Members {
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

func (c *ClusterMemberController) setScaleAnnotation(p *corev1.Pod) error {
	if p == nil {
		update, err := c.updateScalingConfigMap("")
		if err != nil {
			return err
		}
		klog.V(2).Infof("%#v", update)
		return nil
	}

	data, err := c.GetScalingData(p)
	if err != nil {
		return err
	}

	scalingDataString, err := json.Marshal(data)
	if err != nil {
		return err
	}

	update, err := c.updateScalingConfigMap(string(scalingDataString))
	if err != nil {
		return err
	}

	c.eventRecorder.Eventf("ScaleUpAnnotationSet", "configmap annotation set: %#v", update)

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
		c.kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Informer().HasSynced,
		c.kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().HasSynced,
		c.operatorConfigClient.Informer().HasSynced) {
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
	// do we need to check if the member is alreay part of the cluster here?
	resp, err := cli.MemberAdd(context.Background(), peerURLs)
	if err != nil {
		return err
	}
	c.eventRecorder.Eventf("EtcdMemberAdded", "added member with PeerURLs:%s to etcd api", resp.Member.PeerURLs)
	return nil
}

func (c *ClusterMemberController) RemoveBootstrap() error {
	err := c.RemoveBootstrapFromEndpoint()
	if err != nil {
		return err
	}
	return c.EtcdMemberRemove("etcd-bootstrap")
}

func (c *ClusterMemberController) RemoveBootstrapFromEndpoint() error {
	hostEndpoint, err := c.clientset.CoreV1().
		Endpoints(EtcdEndpointNamespace).
		Get(EtcdHostEndpointName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting endpoint: %#v\n", err)
		return err
	}

	hostEndpointCopy := hostEndpoint.DeepCopy()

	subsetIndex := -1
	bootstrapIndex := -1
	for sI, s := range hostEndpointCopy.Subsets {
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

	if len(hostEndpointCopy.Subsets[subsetIndex].Addresses) <= 1 {
		return fmt.Errorf("only etcd-bootstrap endpoint observed, try again")
	}

	hostEndpointCopy.Subsets[subsetIndex].Addresses = append(hostEndpointCopy.Subsets[subsetIndex].Addresses[0:bootstrapIndex], hostEndpointCopy.Subsets[subsetIndex].Addresses[bootstrapIndex+1:]...)

	_, err = c.clientset.CoreV1().Endpoints(EtcdEndpointNamespace).Update(hostEndpointCopy)
	if err != nil {
		klog.Errorf("error updating endpoint: %#v\n", err)
		return err
	}

	return nil
}

func (c *ClusterMemberController) isClusterEtcdOperatorReady() bool {
	pendingMembers, err := c.EtcdList("pending")
	if err != nil {
		klog.Errorf("error getting pending members: %#v", err)
		return false
	}
	if len(pendingMembers) > 0 {
		klog.Infof("some members are pending: %#v", pendingMembers)
		return false
	}
	members, err := c.EtcdList("members")
	if err != nil {
		klog.Errorf("error getting members: %#v", err)
		return false
	}
	if len(members) == 0 {
		klog.Infof("no etcd member found")
		return false
	}
	if len(members) == 1 && c.IsMember("etcd-bootstrap") {
		klog.Infof("etcd-bootstrap is the only known member")
		return false
	}
	return true
}

func (c *ClusterMemberController) getNewMemberToScaleUp() (*corev1.Pod, error) {
	pods, err := c.clientset.CoreV1().Pods("openshift-etcd").List(metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	if err != nil {
		klog.Infof("No Pod found in openshift-etcd with label k8s-app=etcd")
		return nil, err
	}
	for _, p := range pods.Items {
		// if the pod is in any other state and is not a part of the cluster,
		// it should always be restarted via an explicit remove, go through
		// the first init container and pass through here
		if p.Status.Phase == corev1.PodPending && p.Status.InitContainerStatuses[waitForKubeContainerNumber].State.Terminated != nil &&
			p.Status.InitContainerStatuses[waitForKubeContainerNumber].State.Terminated.ExitCode == 0 {
			return &p, nil
		}
	}

	// went through all the pods nothing to scale
	return nil, nil
}

func (c *ClusterMemberController) getNewMemberToScaleDown() (*corev1.Pod, error) {
	members, err := c.EtcdList("members")
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if c.IsStatus("pending", m.Name, ceoapi.MemberRemove) {
			pod, err := c.clientset.CoreV1().Pods("openshift-etcd").Get(m.Name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			klog.V(2).Infof("getNewMemberToScaleDown: scaling down %s", pod.Name)
			return pod, nil
		}
	}
	klog.V(2).Infof("getNewMemberToScaleDown: no member found for scaling down")
	return nil, nil
}

func (c *ClusterMemberController) updateScalingConfigMap(scalingData string) (*corev1.ConfigMap, error) {
	result, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if result.Annotations == nil {
		result.Annotations = make(map[string]string)
	}
	result.Annotations[EtcdScalingAnnotationKey] = scalingData
	update, updateErr := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Update(result)
	if updateErr != nil {
		return nil, updateErr
	}
	return update, nil
}

func (c *ClusterMemberController) GetScalingData(pod *corev1.Pod) (*ceoapi.EtcdScaling, error) {
	// although we dont use SRV for server bootstrap we do use the records to map peerurls
	peerFQDN, err := ReverseLookupSelf("etcd-server-ssl", "tcp", c.etcdDiscoveryDomain, pod.Status.HostIP)
	if err != nil {
		klog.Errorf("error looking up self: %v", err)
		// todo: emit event
		return nil, err
	}

	members, err := c.EtcdList("members")
	if err != nil {
		return nil, err
	}

	return &ceoapi.EtcdScaling{
		Metadata: &metav1.ObjectMeta{
			Name:              pod.Name,
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
		Members: members,
		PodFQDN: peerFQDN,
	}, nil
}

func (c *ClusterMemberController) GetScalingDataFromConfigMap() (*ceoapi.EtcdScaling, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return GetScalingAnnotation(cm)
}

func (c *ClusterMemberController) setNumberOfEtcdMembers() error {
	members, err := c.EtcdList("members")
	if err != nil {
		return err
	}
	c.numberOfEtcdMembers = 0
	for _, m := range members {
		if m.Name != "etcd-bootstrap" {
			c.numberOfEtcdMembers += 1
		}
	}
	return nil
}

package clustermembercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation/etcd"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1client "k8s.io/client-go/kubernetes"
)

const (
	workQueueKey             = "key"
	EtcdScalingAnnotationKey = "etcd.operator.openshift.io/scale"
	etcdCertFile             = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile              = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile        = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
)

type ClusterMemberController struct {
	clientset            corev1client.Interface
	operatorConfigClient v1helpers.OperatorClient
	queue                workqueue.RateLimitingInterface
	eventRecorder        events.Recorder
	etcdDiscoveryDomain  string
}

func NewClusterMemberController(
	clientset corev1client.Interface,
	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
	etcdDiscoveryDomain string,
) *ClusterMemberController {
	c := &ClusterMemberController{
		clientset:            clientset,
		operatorConfigClient: operatorConfigClient,
		queue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController"),
		eventRecorder:        eventRecorder.WithComponentSuffix("cluster-member-controller"),
		etcdDiscoveryDomain:  etcdDiscoveryDomain,
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Endpoints().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ClusterMemberController) sync() error {
	pods, err := c.clientset.CoreV1().Pods("openshift-etcd").List(metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	if err != nil {
		klog.Infof("No Pod found in openshift-etcd with label k8s-app=etcd")
		return err
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		klog.Infof("Found etcd Pod with name %v\n", p.Name)

		if c.IsMemberRemove(p.Name) {
			klog.Infof("Member is unhealthy and is being removed: %s\n", p.Name)
			if err := c.etcdMemberRemove(p.Name); err != nil {
				c.eventRecorder.Warning("ScalingDownFailed", err.Error())
				return err
				// Todo alaypatel07:  need to take care of condition degraded
				// Todo alaypatel07: need to skip this reconciliation loop and continue later
				// after the member is removed from this very point.
			}
		}

		if c.IsMember(p.Name) {
			klog.Infof("Member is already part of the cluster %s\n", p.Name)
			name, err := c.getScaleAnnotationName()
			if err != nil {
				klog.Errorf("failed to obtain name from annotation %v", err)
			}
			// clear annotation
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

		//TODO probalby need some breaks here so we don't scale if degraded.
		members, err := c.MemberList()
		if err != nil {
			return err
		}

		// scale
		// although we dont use SRV for server bootstrap we do use the records to map peerurls
		peerFQDN, err := reverseLookupSelf("etcd-server-ssl", "tcp", c.etcdDiscoveryDomain, p.Status.HostIP)
		if err != nil {
			klog.Errorf("error looking up self: %v", err)
			continue
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

		// start scaling
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := c.setScaleAnnotation(string(esb)); err != nil {
				return err
			}
			return nil
		})
		if retryErr != nil {
			return fmt.Errorf("Update approve failed: %v", retryErr)
		}

		// doing the pause is a hack lets wait for certs to finish?
		// after certs runs we will block until we get the next command from configmap.
		// block until we see running status
		duration := 10 * time.Second
		wait.PollInfinite(duration, func() (bool, error) {
			result, _ := c.clientset.CoreV1().Pods("openshift-etcd").Get(p.Name, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("error sending ClusterMember request: %v", err)
				return false, nil
			}
			if result.Status.Phase != "Running" {
				klog.Infof("Waiting for Pod %s to start", p.Name)
				return false, nil
			}

			return true, nil
		})

		if err := c.etcdMemberAdd([]string{fmt.Sprintf("https://%s:2380", peerFQDN)}); err != nil {
			c.eventRecorder.Warning("ScalingFailed", err.Error())
			return err
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

func (c *ClusterMemberController) MemberList() ([]ceoapi.Member, error) {
	configPath := []string{"cluster", "members"}
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

func (c *ClusterMemberController) PendingMemberList() ([]ceoapi.Member, error) {
	configPath := []string{"cluster", "pending"}
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
	members, _ := c.MemberList()
	for _, m := range members {
		if m.Name == name {
			return true
		}
	}
	return false
}

func (c *ClusterMemberController) IsMemberRemove(name string) bool {
	members, _ := c.PendingMemberList()
	for _, m := range members {
		klog.Warningf("IsMemberRemove: checking %v vs %v type = %v\n", m.Name, name, m.Conditions[0].Type)
		if m.Name == name && m.Conditions[0].Type == ceoapi.MemberRemove {
			return true
		}
	}
	return false
}

func (c *ClusterMemberController) isPodCrashLoop(name string) bool {
	restartCount := make(map[string]int32)
	timeout := 120 * time.Second
	interval := 5 * time.Second

	// check if the pod is activly crashlooping
	if err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		pod, err := c.clientset.CoreV1().Pods("openshift-etcd").Get(name, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("unable to find pod %s: %v. Retrying.", name, err)
			return false, nil
		}
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Waiting == nil {
				continue
			}
			if restartCount[containerStatus.Name] == 0 {
				restartCount[containerStatus.Name] = containerStatus.RestartCount
			}

			if containerStatus.State.Waiting.Reason == "CrashLoopBackOff" {
				if restartCount[containerStatus.Name] > 0 && containerStatus.RestartCount > restartCount[containerStatus.Name] {
					klog.Warningf("found container %s actively in CrashLoopBackOff\n", containerStatus.Name)
					return true, nil
				}
				restartCount[containerStatus.Name] = containerStatus.RestartCount
				return false, nil
			}
		}
		return false, nil
	}); err != nil {
		return false
	}
	return true
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
	result, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	scaling := &ceoapi.EtcdScaling{}
	data, ok := result.Annotations[EtcdScalingAnnotationKey]
	if !ok {
		return "", fmt.Errorf("scaling annotation not found")
	}
	if err := json.Unmarshal([]byte(data), scaling); err != nil {
		klog.Infof("unable to unmarshal scaling data %#v\n", err)
		return "", err
	}
	if scaling.Metadata.Name == "" {
		return "", fmt.Errorf("scaling annotation name not found")
	}
	return scaling.Metadata.Name, nil
}

func reverseLookupSelf(service, proto, name, self string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == self {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}

func (c *ClusterMemberController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController")
	defer klog.Infof("Shutting down ClusterMemberController")

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
		Endpoints(etcd.EtcdEndpointNamespace).
		Get(etcd.EtcdHostEndpointName, metav1.GetOptions{})
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

	_, err = c.clientset.CoreV1().Endpoints(etcd.EtcdEndpointNamespace).Update(hostEndpoint)
	if err != nil {
		klog.Errorf("error updating endpoint: %#v\n", err)
		return err
	}
	return nil

}

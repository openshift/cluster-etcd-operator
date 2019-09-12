package clustermembercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

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

	return c
}

type EtcdScaling struct {
	Metadata *metav1.ObjectMeta `json:"metadata,omitempty"`
	Members  []Member           `json:"members,omitempty"`
}

type Member struct {
	ID         uint64   `json:"ID,omitempty"`
	Name       string   `json:"name,omitempty"`
	PeerURLS   []string `json:"peerURLs,omitempty"`
	ClientURLS []string `json:"clientURLs,omitempty"`
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

		if c.IsMember(p.Name) {
			klog.Infof("Member is already part of the cluster %s\n", p.Name)
			continue
		}

		members, err := c.MemberList()
		if err != nil {
			return err
		}
		es := EtcdScaling{
			Metadata: &metav1.ObjectMeta{
				Name:              p.Name,
				CreationTimestamp: metav1.Time{Time: time.Now()},
			},
			Members: members,
		}

		esb, err := json.Marshal(es)
		if err != nil {
			return err
		}

		// start scaling
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, err := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Get("member-config", metav1.GetOptions{})
			if err != nil {
				return err
			}
			if result.Annotations == nil {
				result.Annotations = make(map[string]string)
			}
			result.Annotations[EtcdScalingAnnotationKey] = string(esb)
			_, updateErr := c.clientset.CoreV1().ConfigMaps("openshift-etcd").Update(result)
			return updateErr
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

		// TODO break out to func
		endpoints, err := c.Endpoints()
		if err != nil {
			return err
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
			return err
		}
		defer cli.Close()

		// scale
		// although we dont use SRV for server bootstrap we do use the records to map peerurls
		peerFQDN, err := reverseLookupSelf("etcd-server-ssl", "tcp", c.etcdDiscoveryDomain, p.Status.HostIP)
		if err != nil {
			klog.Errorf("error looking up self: %v", err)
			continue
		}
		if err := etcdMemberAdd(cli, []string{fmt.Sprintf("https://%s:2380", peerFQDN)}); err != nil {
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

func (c *ClusterMemberController) MemberList() ([]Member, error) {
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
	var members []Member
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

		m := Member{
			Name:     name,
			PeerURLS: []string{peerURLs},
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

func etcdMemberAdd(cli *clientv3.Client, peerURLs []string) error {
	defer cli.Close()
	resp, err := cli.MemberAdd(context.Background(), peerURLs)
	if err != nil {
		return err
	}
	klog.Infof("added etcd member.PeerURLs:%s", resp.Member.PeerURLs)
	return nil
}

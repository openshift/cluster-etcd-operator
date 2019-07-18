package clustermembercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/davecgh/go-spew/spew"
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
	podClient            corev1client.Interface
	operatorConfigClient v1helpers.OperatorClient
	queue                workqueue.RateLimitingInterface
	eventRecorder        events.Recorder
}

func NewClusterMemberController(
	podClient corev1client.Interface,
	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *ClusterMemberController {
	c := &ClusterMemberController{
		podClient:            podClient,
		operatorConfigClient: operatorConfigClient,
		queue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController"),
		eventRecorder:        eventRecorder.WithComponentSuffix("cluster-member-controller"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())

	return c
}

type EtcdScaling struct {
	Metadata *metav1.ObjectMeta     `json:"metadata,omitempty"`
	Members  []*etcdserverpb.Member `json:"members,omitempty"`
}

func (c *ClusterMemberController) sync() error {
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

	pods, err := c.podClient.CoreV1().Pods("openshift-etcd").List(metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	if err != nil {
		klog.Infof("No Pod found in openshift-etcd with label k8s-app=etcd")
		return err
	}

	for i := range pods.Items {
		p := &pods.Items[i]
		klog.Infof("Found etcd Pod with name %v\n", p.Name)

		str := spew.Sdump(p)
		klog.Infof("Pod Data %v\n", str)

		cm, err := c.podClient.CoreV1().ConfigMaps("openshift-etcd").Get("scaling-lock", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cm.Annotations == nil {
			cm.Annotations = make(map[string]string)
		}

		if isMember(cli, p.Name) {
			klog.Infof("Member is already part of the cluster: %s\n", p.Name)
			return nil
		}

		members, err := etcdMemberList(cli)
		if err != nil {
			return err
		}
		es := EtcdScaling{
			Metadata: &metav1.ObjectMeta{
				Name: p.Name,
			},
			Members: members,
		}

		esb, err := json.Marshal(es)
		if err != nil {
			return err
		}

		// start scaling
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			result, err := c.podClient.CoreV1().ConfigMaps("openshift-etcd").Get("scaling-lock", metav1.GetOptions{})
			if err != nil {
				return err
			}
			result.Annotations[EtcdScalingAnnotationKey] = string(esb)
			_, updateErr := c.podClient.CoreV1().ConfigMaps("openshift-etcd").Update(result)
			return updateErr
		})
		if retryErr != nil {
			return fmt.Errorf("Update approve failed: %v", retryErr)
		}
		// scale
		if err := etcdMemberAdd(cli, []string{p.Status.HostIP}); err != nil {
			return err
		}

		if isMember(cli, p.Name) {
			klog.Infof("Member is already part of the cluster: %s\n", p.Name)
			return nil
		}
		rerr := fmt.Errorf("failed to observe new member")
		c.eventRecorder.Warning("ScalingInProgress", rerr.Error())
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
	endpoints, found, err := unstructured.NestedStringSlice(config, storageConfigURLsPath...)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("etcd storageConfig urls not observed")
	}

	return []string{endpoints[0]}, nil
}

func isMember(cli *clientv3.Client, name string) bool {
	members, err := etcdMemberList(cli)
	if err != nil {
		klog.V(4).Infof("etcd member list failed with error: %v", err)
		return false
	}
	for _, m := range members {
		if m.Name == name {
			return true
		}
	}
	return false
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
	fmt.Errorf("runWorker %v", c)
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

func createClientv3(cfg *clientv3.Config) (*clientv3.Client, error) {
	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return cli, nil
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

func etcdMemberList(cli *clientv3.Client) ([]*etcdserverpb.Member, error) {
	resp, err := cli.MemberList(context.Background())
	if err != nil {
		return nil, err
	}
	return resp.Members, nil
}

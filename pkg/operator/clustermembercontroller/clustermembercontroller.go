package clustermembercontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/davecgh/go-spew/spew"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	etcdv1 "github.com/openshift/api/etcd/v1"
	etcdv1client "github.com/openshift/client-go/etcd/clientset/versioned/typed/etcd/v1"
	etcdv1informer "github.com/openshift/client-go/etcd/informers/externalversions"
	corev1informer "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
)

const workQueueKey = "key"

type ClusterMemberController struct {
	podClient            corev1client.Interface
	podLister            corev1lister.PodLister
	podSynced            cache.InformerSynced
	podInformer          corev1informer.PodInformer
	etcdClient           etcdv1client.ClusterMemberRequestInterface
	etcdInformer         etcdv1informer.SharedInformerFactory
	operatorConfigClient v1helpers.OperatorClient
	queue                workqueue.RateLimitingInterface
	eventRecorder        events.Recorder
}

func NewClusterMemberController(
	podClient corev1client.Interface,
	podInformer corev1informer.PodInformer,

	etcdClient etcdv1client.ClusterMemberRequestInterface,
	etcdInformer etcdv1informer.SharedInformerFactory,

	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *ClusterMemberController {
	c := &ClusterMemberController{
		podClient:            podClient,
		etcdClient:           etcdClient,
		etcdInformer:         etcdInformer,
		podLister:            podInformer.Lister(),
		podSynced:            podInformer.Informer().HasSynced,
		podInformer:          podInformer,
		operatorConfigClient: operatorConfigClient,
		queue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController"),
		eventRecorder:        eventRecorder.WithComponentSuffix("cluster-member-controller"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	etcdInformer.Etcd().V1().ClusterMemberRequests().Informer().AddEventHandler(c.eventHandler())
	// podInformer.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *ClusterMemberController) sync() error {
	pods, err := c.podClient.CoreV1().Pods("openshift-etcd").List(metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	if err != nil {
		return err
		klog.Infof("Found no Pod in openshift-etcd with label k8s-app=etcd")
	}

	//TODO break out logic into functions.
	for i := range pods.Items {
		p := &pods.Items[i]
		klog.Infof("Found etcd Pod with name %v\n", p.Name)

		str := spew.Sdump(p)
		klog.Infof("Pod Data %v\n", str)
		// check if we have a request already
		result, err := c.etcdClient.Get(p.Name, metav1.GetOptions{})
		// if not create
		if errors.IsNotFound(err) {
			rstr := spew.Sdump(result)
			klog.Infof("Request Data %v\n", rstr)
			cm := &etcdv1.ClusterMemberRequest{
				metav1.TypeMeta{
					Kind:       "ClusterMemberRequest",
					APIVersion: "etcd.openshift.io/v1",
				},
				metav1.ObjectMeta{
					Name: p.Name,
				},
				etcdv1.ClusterMemberRequestSpec{
					Name: p.Name,
				},
				etcdv1.ClusterMemberRequestStatus{},
			}
			cmr, err := c.etcdClient.Create(cm)
			if err != nil {
				klog.Errorf("error sending ClusterMembeRequest: %v", err)
				return err
			}
			klog.Infof("New ClusterMemberRequest created %v\n", cmr)
			return nil

		} else if err != nil {
			return err
		}

		// block until PeerURLs is updated by client
		if result.Spec.PeerURLs == "" {
			err := fmt.Errorf("no PeerURLs observed")
			c.eventRecorder.Warning("ClusterMemberRequest", err.Error())
			return err
		}

		// is approved?
		if exists := conditionContains(result.Status.Conditions, etcdv1.ClusterMemberApproved); exists == false {
			retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				result, err := c.etcdClient.Get(p.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if exists := conditionContains(result.Status.Conditions, etcdv1.ClusterMemberApproved); exists == false {
					// clear the path for next step we designate Approved
					result.Status.Conditions = []etcdv1.ClusterMemberRequestCondition{
						etcdv1.ClusterMemberRequestCondition{
							Type: etcdv1.ClusterMemberApproved,
						},
					}
				}
				return nil
			})
			if retryErr != nil {
				return fmt.Errorf("Update failed: %v", retryErr)
			}
		}
		// is delivered?
		if exists := conditionContains(result.Status.Conditions, etcdv1.ClusterMemberDelivered); exists == false {
			err := fmt.Errorf("client has not reported ClusterMember config as delivered")
			c.eventRecorder.Warning("ClusterMemberRequest", err.Error())
			return err
		}
		// here we should make sure there is only one queued up. we probably should never see more than one request approved if this works right.
		// if we do we should panic as we are heading for a world of pain.

		endpoints, err := c.Endpoints()
		if err != nil {
			return err
		}

		//TODO check that we have already synced and these are not still just stubs
		tlsInfo := transport.TLSInfo{
			CertFile:      "/var/run/secrets/etcd-client/tls.crt",
			KeyFile:       "/var/run/secrets/etcd-client/tls.key",
			TrustedCAFile: "/run/configmaps/etcd-ca/ca-bundle.crt",
		}
		tlsConfig, err := tlsInfo.ClientConfig()

		cfg := &clientv3.Config{
			Endpoints:   endpoints,
			DialTimeout: 5 * time.Second,
			TLS:         tlsConfig,
		}
		cli, err := createClientv3(cfg)
		if err != nil {
			return err
		}
		defer cli.Close()
		// create a scaling lock.
		s, err := concurrency.NewSession(cli)
		defer s.Close()
		scaling := concurrency.NewMutex(s, "/openshift.io/etcd/scaling/lock/")
		if err := scaling.Lock(context.TODO()); err != nil {
			return err
		}
		klog.Infof("scaling lock aquired %v\n", scaling)

		// TODO this does the actual scale up, uncomment to complete pivot
		// if err := etcdMemberAdd(cli, peerUrls); err != nil {
		// 	return err
		// }

		if _, err := etcdMemberList(cli); err != nil {
			return err
		}
		if err := scaling.Unlock(context.TODO()); err != nil {
			return err
		}

	}
	return nil
}

func (c *ClusterMemberController) Endpoints() ([]string, error) {
	endpoints := []string{}
	// populate etcd client endpoints
	operatorSpec, _, _, err := c.operatorConfigClient.GetOperatorState()
	if err != nil {
		return endpoints, err
	}
	existingConfig := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(operatorSpec.ObservedConfig.Raw)).Decode(&existingConfig); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}
	rawEndpoints := existingConfig["cluster"].(map[string]interface{})["peers"].([]interface{})
	// cast to []string
	// endpoints := make([]string, len(rawEndpoints))
	for i, _ := range rawEndpoints {
		endpoints[i] = fmt.Sprintf("https://%s:%d", rawEndpoints[i], 2379)
	}
	return endpoints, nil
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

func conditionContains(conditions []etcdv1.ClusterMemberRequestCondition, value etcdv1.RequestConditionType) bool {
	for _, c := range conditions {
		if c.Type == value {
			return true
		}
	}
	return false
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

func etcdMemberList(cli *clientv3.Client) ([]string, error) {
	members := []string{}
	resp, err := cli.MemberList(context.Background())
	if err != nil {
		return members, err
	}
	for _, memb := range resp.Members {
		for _, u := range memb.PeerURLs {
			n := memb.Name
			members = append(members, fmt.Sprintf("%s=%s", n, u))
		}
	}
	klog.Infof("current etcd member: %s", members)
	return members, nil
}

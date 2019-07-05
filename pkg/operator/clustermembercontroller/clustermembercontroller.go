package clustermembercontroller

import (
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	etcdv1 "github.com/openshift/api/etcd/v1"
	etcdv1client "github.com/openshift/client-go/etcd/clientset/versioned/typed/etcd/v1"
	etcdv1informer "github.com/openshift/client-go/etcd/informers/externalversions"
	corev1informer "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1lister "k8s.io/client-go/listers/core/v1"
)

const workQueueKey = "key"

type ClusterMemberController struct {
	podClient     corev1client.PodsGetter
	podLister     corev1lister.PodLister
	podSynced     cache.InformerSynced
	podInformer   corev1informer.PodInformer
	etcdClient    etcdv1client.ClusterMemberRequestInterface
	etcdInformer  etcdv1informer.SharedInformerFactory
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewClusterMemberController(
	podClient corev1client.PodsGetter,
	podInformer corev1informer.PodInformer,

	etcdClient etcdv1client.ClusterMemberRequestInterface,
	etcdInformer etcdv1informer.SharedInformerFactory,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *ClusterMemberController {
	c := &ClusterMemberController{
		podClient:     podClient,
		etcdClient:    etcdClient,
		etcdInformer:  etcdInformer,
		podLister:     podInformer.Lister(),
		podSynced:     podInformer.Informer().HasSynced,
		podInformer:   podInformer,
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ClusterMemberController"),
		eventRecorder: eventRecorder.WithComponentSuffix("cluster-member-controller"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	etcdInformer.Etcd().V1().ClusterMemberRequests().Informer().AddEventHandler(c.eventHandler())
	// podInformer.Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *ClusterMemberController) sync() error {
	pods, err := c.podClient.Pods("openshift-etcd").List(metav1.ListOptions{LabelSelector: "k8s-app=etcd"})
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		klog.Infof("Found no Pod in openshift-etcd with label k8s-app=etcd")
	}

	//TODO break out logic into functions.
	for i := range pods.Items {
		p := &pods.Items[i]
		klog.Infof("Found etcd Pod with name %v\n", p.Name)

		str := spew.Sdump(p)
		klog.Infof("Pod Data %v\n", str)
		// check if we have a request already
		request, err := c.etcdClient.Get(p.Name, metav1.GetOptions{})
		// if not create
		if errors.IsNotFound(err) {
			rstr := spew.Sdump(request)
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

		// block until config is observed and specific paths are present
		if request.Spec.PeerURLs == "" {
			err := fmt.Errorf("no PeerURLs observed")
			c.eventRecorder.Warning("ClusterMemberRequest", err.Error())
			return err
		}
		// clear the path for next step we designate Approved
		request.Status.Conditions = []etcdv1.ClusterMemberRequestCondition{
			etcdv1.ClusterMemberRequestCondition{
				Type: etcdv1.ClusterMemberApproved,
			},
		}
		if _, err := c.etcdClient.Update(request); err != nil {
			return err
		}

	}

	return nil
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

package staticsynccontroller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	etcdv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
)

const (
	workQueueKey  = "key"
	srcDir        = "/var/run/secrets/kubernetes.io/serviceaccount"
	destDir       = "/run/secrets/etcd"
	etcdNamespace = "openshift-etcd"
)

type syncOpts struct {
	errOut io.Writer
}

// NewStaticSyncCommand creates a staticsync controller.
func NewStaticSyncCommand(errOut io.Writer) *cobra.Command {
	syncOpts := &syncOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "staticsync",
		Short: "syncs assets for etcd",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(syncOpts.errOut, err.Error())
				}
			}
			must(syncOpts.Run)
		},
	}

	syncOpts.AddFlags(cmd.Flags())
	return cmd
}

func (s *syncOpts) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
}

func (s *syncOpts) Run() error {
	info := version.Get()

	// To help debugging, immediately log version
	klog.Infof("Version: %+v (%s)", info.GitVersion, info.GitCommit)

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	operatorConfigClient, err := operatorversionedclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	operatorConfigInformers := operatorv1informers.NewSharedInformerFactory(operatorConfigClient, 10*time.Minute)
	operatorClient := &operatorclient.OperatorClient{
		Informers: operatorConfigInformers,
		Client:    operatorConfigClient.OperatorV1(),
	}

	//TODO: util v6j
	controllerRef, err := events.GetControllerReferenceForCurrentPod(clientset, etcdNamespace, nil)
	if err != nil {
		klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
	}

	eventRecorder := events.NewKubeRecorder(clientset.CoreV1().Events(etcdNamespace), "resource-sync-controller-"+os.Getenv("NODE_NAME"), controllerRef)

	kubeInformerFactory := informers.NewFilteredSharedInformerFactory(clientset, 0, etcdNamespace, nil)

	etcdInformer, err := operatorClient.Informers.ForResource(schema.GroupVersionResource{
		Group:    "operator.openshift.io",
		Version:  "v1",
		Resource: "etcds",
	})
	if err != nil {
		klog.Errorf("error getting etcd informer %#v", err)
		return err
	}

	staticSyncController := NewStaticSyncController(
		operatorClient.Client.Etcds(),
		etcdInformer,
		kubeInformerFactory,
		eventRecorder,
	)

	kubeInformerFactory.Start(ctx.Done())

	go staticSyncController.Run(ctx.Done())

	<-ctx.Done()
	return fmt.Errorf("stopped")
}

type StaticSyncController struct {
	etcdKubeClient                         etcdv1.EtcdInterface
	etcdInformer                           informers.GenericInformer
	secretInformer                         cache.SharedIndexInformer
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewStaticSyncController(
	etcdKubeClient etcdv1.EtcdInterface,
	etcdInformer informers.GenericInformer,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *StaticSyncController {
	c := &StaticSyncController{
		etcdKubeClient:                         etcdKubeClient,
		etcdInformer:                           etcdInformer,
		secretInformer:                         kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer(),
		kubeInformersForOpenshiftEtcdNamespace: kubeInformersForOpenshiftEtcdNamespace,
		eventRecorder:                          eventRecorder.WithComponentSuffix("resource-sync-controller-" + os.Getenv("NODE_NAME")),
		queue:                                  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ResourceSyncController"),
	}
	c.secretInformer.AddEventHandler(c.eventHandler())

	return c
}

func (c *StaticSyncController) sync() error {
	// if anything changes we copy
	assets := [3]string{
		"namespace",
		"ca.crt",
		"token",
	}
	errs := []error{}
	for _, file := range assets {
		if err := Copy(fmt.Sprintf("%s/%s", srcDir, file), fmt.Sprintf("%s/%s", destDir, file)); err != nil {
			klog.Errorf("error copying file %s: %#v", file, err)
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errors.New(fmt.Sprintf("error copying resources: %#v", errs))
	}
	klog.Info("resources synced successfully")
	return nil
}

func Copy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Close()
}

func (c *StaticSyncController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting StaticSyncController")
	defer klog.Infof("Shutting down StaticSyncController")

	if !cache.WaitForCacheSync(stopCh,
		c.secretInformer.HasSynced,
	) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *StaticSyncController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *StaticSyncController) processNextWorkItem() bool {
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
func (c *StaticSyncController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

package staticsynccontroller

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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
	corev1informer "k8s.io/client-go/informers/core/v1"
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

	staticSyncController := NewStaticSyncController(
		operatorClient,
		kubeInformerFactory,
		eventRecorder,
	)

	kubeInformerFactory.Start(ctx.Done())

	go staticSyncController.Run(ctx.Done())

	<-ctx.Done()
	return fmt.Errorf("stopped")
}

type StaticSyncController struct {
	operatorConfigClient                   v1helpers.OperatorClient
	podInformer                            corev1informer.SecretInformer
	kubeInformersForOpenshiftEtcdNamespace cache.SharedIndexInformer

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewStaticSyncController(
	operatorConfigClient v1helpers.OperatorClient,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *StaticSyncController {
	c := &StaticSyncController{
		operatorConfigClient: operatorConfigClient,
		eventRecorder:        eventRecorder.WithComponentSuffix("resource-sync-controller-" + os.Getenv("NODE_NAME")),
		queue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ResourceSyncController"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Secrets().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *StaticSyncController) sync() error {
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
	// if anything changes we copy
	assets := [4]string{
		"namespace",
		"ca.crt",
		"service-ca.crt",
		"token",
	}
	for _, file := range assets {
		if err := Copy(fmt.Sprintf("%s/%s", srcDir, file), fmt.Sprintf("%s/%s", destDir, file)); err != nil {
			return err
		}
	}
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

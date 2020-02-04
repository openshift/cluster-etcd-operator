package staticpodcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"time"

	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	etcdv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/vincent-petithory/dataurl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey       = "key"
	manifestDir        = "/etc/kubernetes/manifests"
	dataDir            = "/var/lib/etcd"
	etcdMemberFileName = "etcd-member.yaml"
	etcdNamespace      = "openshift-etcd"
)

type podOpts struct {
	errOut io.Writer
}

// NewStaticSyncCommand creates a staticpod controller.
func NewStaticPodCommand(errOut io.Writer) *cobra.Command {
	podOpts := &podOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "staticpod",
		Short: "static pod controller takes physical actions against static pods",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(podOpts.errOut, err.Error())
				}
			}
			must(podOpts.Run)
		},
	}

	podOpts.AddFlags(cmd.Flags())
	return cmd
}

func (s *podOpts) AddFlags(fs *pflag.FlagSet) {
	fs.Set("logtostderr", "true")
}

func (s *podOpts) Run() error {
	info := version.Get()

	// To help debugging, immediately log version
	klog.Infof("Version: %+v (%s)", info.GitVersion, info.GitCommit)

	// here
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
	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	operatorConfigInformers := operatorv1informers.NewSharedInformerFactory(operatorConfigClient, 10*time.Minute)
	operatorClient := &operatorclient.OperatorClient{
		Informers: operatorConfigInformers,
		Client:    operatorConfigClient.OperatorV1(),
	}

	kubeInformerFactory := informers.NewFilteredSharedInformerFactory(clientset, 0, etcdNamespace, nil)
	nodeName := os.Getenv("NODE_NAME")
	var localEtcdName string
	if nodeName != "" {
		localEtcdName = fmt.Sprintf("etcd-member-%s", nodeName)
	}

	//TODO: util v6j
	controllerRef, err := events.GetControllerReferenceForCurrentPod(clientset, etcdNamespace, nil)
	if err != nil {
		klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
	}

	eventRecorder := events.NewKubeRecorder(clientset.CoreV1().Events(etcdNamespace), "static-pod-controller-"+localEtcdName, controllerRef)

	if err != nil {
		klog.Errorf("error getting etcd informer %#v\n", err)
		return err
	}

	staticPodController := NewStaticPodController(
		operatorClient.Client.Etcds(),
		operatorConfigInformers.Operator().V1().Etcds(),
		kubeInformerFactory,
		clientset,
		dynamicClient,
		localEtcdName,
		eventRecorder,
	)

	kubeInformerFactory.Start(ctx.Done())
	operatorClient.Informers.Start(ctx.Done())

	go staticPodController.Run(ctx.Done())

	<-ctx.Done()
	return fmt.Errorf("stopped")
}

type StaticPodController struct {
	etcdKubeClient                         etcdv1.EtcdInterface
	etcdInformer                           operatorinformers.EtcdInformer
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory
	clientset                              corev1client.Interface
	localEtcdName                          string
	dynamicClient                          dynamic.Interface

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewStaticPodController(
	etcdKubeClient etcdv1.EtcdInterface,
	etcdInformer operatorinformers.EtcdInformer,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	clientset corev1client.Interface,
	dynamicClient dynamic.Interface,
	localEtcdName string,
	eventRecorder events.Recorder,
) *StaticPodController {
	c := &StaticPodController{
		etcdKubeClient:                         etcdKubeClient,
		etcdInformer:                           etcdInformer,
		eventRecorder:                          eventRecorder.WithComponentSuffix("static-pod-controller-" + localEtcdName),
		kubeInformersForOpenshiftEtcdNamespace: kubeInformersForOpenshiftEtcdNamespace,
		clientset:                              clientset,
		dynamicClient:                          dynamicClient,
		localEtcdName:                          localEtcdName,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "StaticPodController"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
	etcdInformer.Informer().AddEventHandler(c.eventHandler())
	// kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *StaticPodController) sync() error {
	pod, err := c.clientset.CoreV1().Pods(etcdNamespace).Get(c.localEtcdName, metav1.GetOptions{})
	if err != nil {
		klog.Infof("No Pod found in %s with name %s", etcdNamespace, c.localEtcdName)
		return err
	}
	staticPodPath := fmt.Sprintf("%s/%s", manifestDir, etcdMemberFileName)
	// If last status is CrashLoopBackOff we perform further inspection to verify.
	if len(pod.Status.ContainerStatuses) > 0 {
		if pod.Status.ContainerStatuses[0].State.Waiting != nil && pod.Status.ContainerStatuses[0].State.Waiting.Reason == "CrashLoopBackOff" {

			klog.Infof("%s is unhealthy", c.localEtcdName)

			// if the data-dir is missing we are going to give it some time to recover.
			if _, err := os.Stat(fmt.Sprintf("%s/member/snap", dataDir)); os.IsNotExist(err) {
				klog.Warningf("data-dir already removed %s", dataDir)
				return nil
			}

			if c.IsMemberRemove(c.localEtcdName) {
				klog.Infof("%s is pending removal", c.localEtcdName)
				etcdMember, err := c.getMachineConfigData(staticPodPath, "master")
				if err != nil {
					klog.Warningf("etcdMember failed %v", err)
					return err
				}
				klog.Infof("removing static pod %s\n", staticPodPath)
				if err := os.Remove(staticPodPath); err != nil {
					return err
				}

				//TODO verify etcd-member container has exited instead of sleep
				klog.Infof("sleeping for %d\n", 10)
				time.Sleep(10 * time.Second)

				klog.Infof("removing data-dir contents")
				os.RemoveAll(fmt.Sprintf("%s/member", dataDir))
				klog.Infof("starting %s", c.localEtcdName)
				if err := ioutil.WriteFile(staticPodPath, etcdMember, 0644); err != nil {
					klog.Errorf("error starting pod %s: %#v", c.localEtcdName, err)
					return err
				}
			}
		}
	}
	return nil
}

func (c *StaticPodController) IsMemberRemove(name string) bool {
	members, err := c.PendingMemberList()
	if err != nil {
		klog.Errorf("IsMemberRemove: error %v", err)
	}
	for _, m := range members {
		klog.Warningf("IsMemberRemove: checking %v vs %v type = %v\n", m.Name, name, m.Conditions[0].Type)
		if m.Name == name && m.Conditions[0].Type == ceoapi.MemberRemove {
			return true
		}
	}
	return false
}

func (c *StaticPodController) PendingMemberList() ([]ceoapi.Member, error) {
	configPath := []string{"cluster", "pending"}
	etcd, err := c.etcdKubeClient.Get("cluster", metav1.GetOptions{})
	if err != nil {
		fmt.Printf("error getting etcd cr: %#v", err)
		return nil, err
	}

	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(etcd.Spec.ObservedConfig.Raw)).Decode(&config); err != nil {
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

func (c *StaticPodController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Waiting for Cache to sync staticpodcontroller")
	if !cache.WaitForCacheSync(stopCh,
		c.kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().HasSynced,
		c.etcdInformer.Informer().HasSynced,
	) {
		klog.Errorf("error syncing cache")
		return
	}

	klog.Infof("Starting staticpodcontroller")
	defer klog.Infof("Shutting down staticpodcontroller")

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *StaticPodController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *StaticPodController) processNextWorkItem() bool {
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
func (c *StaticPodController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

func (c *StaticPodController) getMachineConfigData(desiredPath string, pool string) ([]byte, error) {
	mcpClient := c.dynamicClient.Resource(schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "machineconfigpools"})
	unstructuredMCP, err := mcpClient.Get(pool, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	machineConfigName, found, err := unstructured.NestedString(unstructuredMCP.Object, "status", "configuration", "name")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("missing machineConfig from %q", pool)
	}
	klog.Infof("rendered master MachineConfig found %s\n", machineConfigName)

	mcClient := c.dynamicClient.Resource(schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "machineconfigs"})
	unstructuredMC, err := mcClient.Get(machineConfigName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	machineConfigFiles, found, err := unstructured.NestedSlice(unstructuredMC.Object, "spec", "config", "storage", "files")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("missing files from machineconfigs/%q", machineConfigName)
	}

	for _, uncastFile := range machineConfigFiles {
		fileMap, ok := uncastFile.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("missing files %T", uncastFile)
		}
		currPath, _, _ := unstructured.NestedString(fileMap, "path")
		if currPath == desiredPath {
			klog.Infof("found path %s\n", desiredPath)

			source, found, err := unstructured.NestedString(fileMap, "contents", "source")
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("missing contents for %q from machineconfigs/%q", currPath, machineConfigName)
			}

			data, err := dataurl.DecodeString(source)
			if err != nil {
				return nil, err
			}
			return data.Data, nil
		}
	}

	return nil, nil
}

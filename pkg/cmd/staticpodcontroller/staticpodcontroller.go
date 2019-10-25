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

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"

	"github.com/openshift/cluster-etcd-operator/pkg/version"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/vincent-petithory/dataurl"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	operatorclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	mcfgclientset "github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1informer "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes"
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
	clientmc, err := mcfgclientset.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	operatorClient, err := operatorclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	etcdOperatorClient := operatorClient.Etcds()

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

	staticPodController := NewStaticPodController(
		etcdOperatorClient,
		kubeInformerFactory,
		clientset,
		clientmc,
		localEtcdName,
		eventRecorder,
	)

	kubeInformerFactory.Start(ctx.Done())

	go staticPodController.Run(ctx.Done())

	<-ctx.Done()
	return fmt.Errorf("stopped")
}

type StaticPodController struct {
	etcdOperatorClient                     operatorclient.EtcdInterface
	podInformer                            corev1informer.SecretInformer
	kubeInformersForOpenshiftEtcdNamespace cache.SharedIndexInformer
	clientset                              corev1client.Interface
	clientmc                               mcfgclientset.Interface
	localEtcdName                          string

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewStaticPodController(
	etcdOperatorClient operatorclient.EtcdInterface,
	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	clientset corev1client.Interface,
	clientmc mcfgclientset.Interface,
	localEtcdName string,
	eventRecorder events.Recorder,
) *StaticPodController {
	c := &StaticPodController{
		etcdOperatorClient: etcdOperatorClient,
		eventRecorder:      eventRecorder.WithComponentSuffix("static-pod-controller-" + localEtcdName),
		clientset:          clientset,
		clientmc:           clientmc,
		localEtcdName:      localEtcdName,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "StaticPodController"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().Pods().Informer().AddEventHandler(c.eventHandler())
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
				if err := ioutil.WriteFile(staticPodPath, []byte(etcdMember), 0644); err != nil {
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
	operatorSpec, err := c.etcdOperatorClient.Get("cluster", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(operatorSpec.Spec.ObservedConfig.Raw)).Decode(&config); err != nil {
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

func (c *StaticPodController) getMachineConfigData(path string, pool string) ([]byte, error) {
	var data []byte
	mcp, err := c.clientmc.MachineconfigurationV1().MachineConfigPools().Get(pool, metav1.GetOptions{})
	if err != nil {
		return data, err
	}
	klog.Infof("rendered master MachineConfig found %s\n", mcp.Status.Configuration.Name)
	mc, err := c.clientmc.MachineconfigurationV1().MachineConfigs().Get(mcp.Status.Configuration.Name, metav1.GetOptions{})
	if err != nil {
		return data, err
	}

	for _, file := range mc.Spec.Config.Storage.Files {
		if file.Path == path {
			klog.Infof("found path %s\n", path)

			data, err := dataurl.DecodeString(file.Contents.Source)
			if err != nil {
				return nil, err
			}
			return data.Data, nil
		}

	}
	return data, nil
}

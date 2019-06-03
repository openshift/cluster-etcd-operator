package certrotationcontroller

import (
	"fmt"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	configlisterv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/certrotation"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// defaultRotationDay is the default rotation base for all cert rotation operations.
const defaultRotationDay = 24 * time.Hour

type CertRotationController struct {
	certRotators []*certrotation.CertRotationController

	networkLister        configlisterv1.NetworkLister
	infrastructureLister configlisterv1.InfrastructureLister

	serviceNetwork        *DynamicServingRotation
	serviceHostnamesQueue workqueue.RateLimitingInterface

	externalLoadBalancer               *DynamicServingRotation
	externalLoadBalancerHostnamesQueue workqueue.RateLimitingInterface

	internalLoadBalancer               *DynamicServingRotation
	internalLoadBalancerHostnamesQueue workqueue.RateLimitingInterface

	cachesToSync []cache.InformerSynced
}

func NewCertRotationController(
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.StaticPodOperatorClient,
	configInformer configinformers.SharedInformerFactory,
	kubeInformersForNamespaces v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
	day time.Duration,
) (*CertRotationController, error) {
	ret := &CertRotationController{
		networkLister:        configInformer.Config().V1().Networks().Lister(),
		infrastructureLister: configInformer.Config().V1().Infrastructures().Lister(),

		serviceHostnamesQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ServiceHostnames"),
		serviceNetwork:        &DynamicServingRotation{hostnamesChanged: make(chan struct{}, 10)},

		externalLoadBalancerHostnamesQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ExternalLoadBalancerHostnames"),
		externalLoadBalancer:               &DynamicServingRotation{hostnamesChanged: make(chan struct{}, 10)},

		internalLoadBalancerHostnamesQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "InternalLoadBalancerHostnames"),
		internalLoadBalancer:               &DynamicServingRotation{hostnamesChanged: make(chan struct{}, 10)},

		cachesToSync: []cache.InformerSynced{
			configInformer.Config().V1().Networks().Informer().HasSynced,
			configInformer.Config().V1().Infrastructures().Informer().HasSynced,
		},
	}

	configInformer.Config().V1().Networks().Informer().AddEventHandler(ret.serviceHostnameEventHandler())
	configInformer.Config().V1().Infrastructures().Informer().AddEventHandler(ret.externalLoadBalancerHostnameEventHandler())

	rotationDay := defaultRotationDay
	if day != time.Duration(0) {
		rotationDay = day
		klog.Warningf("!!! UNSUPPORTED VALUE SET !!!")
		klog.Warningf("Certificate rotation base set to %q", rotationDay)
	}

	certRotator, err := certrotation.NewCertRotationController(
		"AggregatorProxyClientCert",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "aggregator-client-signer",
			Validity:      30 * rotationDay,
			Refresh:       15 * rotationDay,
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.GlobalMachineSpecifiedConfigNamespace,
			Name:          "kube-apiserver-aggregator-client-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.GlobalMachineSpecifiedConfigNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.GlobalMachineSpecifiedConfigNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "aggregator-client",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ClientRotation{
				UserInfo: &user.DefaultInfo{Name: "system:openshift-aggregator"},
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"KubeAPIServerToKubeletClientCert",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-apiserver-to-kubelet-signer",
			Validity:      10 * 365 * rotationDay, // this comes from the installer
			Refresh:       8 * 365 * rotationDay,  // this means we effectively do not rotate
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-apiserver-to-kubelet-client-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "kubelet-client",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ClientRotation{
				UserInfo: &user.DefaultInfo{Name: "system:kube-apiserver", Groups: []string{"kube-master"}},
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"LocalhostServing",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "localhost-serving-signer",
			Validity:      10 * 365 * rotationDay, // this comes from the installer
			Refresh:       8 * 365 * rotationDay,  // this means we effectively do not rotate
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "localhost-serving-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "localhost-serving-cert-certkey",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ServingRotation{
				Hostnames: func() []string { return []string{"localhost", "127.0.0.1"} },
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"ServiceNetworkServing",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "service-network-serving-signer",
			Validity:      10 * 365 * rotationDay, // this comes from the installer
			Refresh:       8 * 365 * rotationDay,  // this means we effectively do not rotate
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "service-network-serving-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "service-network-serving-certkey",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ServingRotation{
				Hostnames:        ret.serviceNetwork.GetHostnames,
				HostnamesChanged: ret.serviceNetwork.hostnamesChanged,
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"ExternalLoadBalancerServing",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "loadbalancer-serving-signer",
			Validity:      10 * 365 * rotationDay, // this comes from the installer
			Refresh:       8 * 365 * rotationDay,  // this means we effectively do not rotate
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "loadbalancer-serving-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "external-loadbalancer-serving-certkey",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ServingRotation{
				Hostnames:        ret.externalLoadBalancer.GetHostnames,
				HostnamesChanged: ret.externalLoadBalancer.hostnamesChanged,
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"InternalLoadBalancerServing",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "loadbalancer-serving-signer",
			Validity:      10 * 365 * rotationDay, // this comes from the installer
			Refresh:       8 * 365 * rotationDay,  // this means we effectively do not rotate
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "loadbalancer-serving-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "internal-loadbalancer-serving-certkey",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ServingRotation{
				Hostnames:        ret.internalLoadBalancer.GetHostnames,
				HostnamesChanged: ret.internalLoadBalancer.hostnamesChanged,
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"KubeControllerManagerClient",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-control-plane-signer",
			Validity:      60 * rotationDay,
			Refresh:       30 * rotationDay,
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-control-plane-signer-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.GlobalMachineSpecifiedConfigNamespace,
			Name:      "kube-controller-manager-client-cert-key",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ClientRotation{
				UserInfo: &user.DefaultInfo{Name: "system:kube-controller-manager"},
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.GlobalMachineSpecifiedConfigNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.GlobalMachineSpecifiedConfigNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"KubeSchedulerClient",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-control-plane-signer",
			Validity:      60 * rotationDay,
			Refresh:       30 * rotationDay,
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-control-plane-signer-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.GlobalMachineSpecifiedConfigNamespace,
			Name:      "kube-scheduler-client-cert-key",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ClientRotation{
				UserInfo: &user.DefaultInfo{Name: "system:kube-scheduler"},
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.GlobalMachineSpecifiedConfigNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.GlobalMachineSpecifiedConfigNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	certRotator, err = certrotation.NewCertRotationController(
		"KubeAPIServerCertSyncer",
		certrotation.SigningRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-control-plane-signer",
			Validity:      60 * rotationDay,
			Refresh:       30 * rotationDay,
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.CABundleRotation{
			Namespace:     operatorclient.OperatorNamespace,
			Name:          "kube-control-plane-signer-ca",
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.OperatorNamespace).Core().V1().ConfigMaps().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		certrotation.TargetRotation{
			Namespace: operatorclient.TargetNamespace,
			Name:      "kube-apiserver-cert-syncer-client-cert-key",
			Validity:  30 * rotationDay,
			Refresh:   15 * rotationDay,
			CertCreator: &certrotation.ClientRotation{
				UserInfo: &user.DefaultInfo{
					Name:   "system:kube-apiserver-cert-syncer",
					Groups: []string{"system:masters"},
				},
			},
			Informer:      kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets(),
			Lister:        kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Lister(),
			Client:        kubeClient.CoreV1(),
			EventRecorder: eventRecorder,
		},
		operatorClient,
	)
	if err != nil {
		return nil, err
	}
	ret.certRotators = append(ret.certRotators, certRotator)

	return ret, nil
}

func (c *CertRotationController) WaitForReady(stopCh <-chan struct{}) {
	klog.Infof("Waiting for CertRotation")
	defer klog.Infof("Finished waiting for CertRotation")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	// need to sync at least once before beginning.  if we fail, we cannot start rotating certificates
	if err := c.syncServiceHostnames(); err != nil {
		panic(err)
	}
	if err := c.syncExternalLoadBalancerHostnames(); err != nil {
		panic(err)
	}
	if err := c.syncInternalLoadBalancerHostnames(); err != nil {
		panic(err)
	}

	for _, certRotator := range c.certRotators {
		certRotator.WaitForReady(stopCh)
	}
}

// RunOnce will run the cert rotation logic, but will not try to update the static pod status.
// This eliminates the need to pass an OperatorClient and avoids dubious writes and status.
func (c *CertRotationController) RunOnce() error {
	errlist := []error{}
	for _, certRotator := range c.certRotators {
		if err := certRotator.RunOnce(); err != nil {
			errlist = append(errlist, err)
		}
	}

	return utilerrors.NewAggregate(errlist)
}

func (c *CertRotationController) Run(workers int, stopCh <-chan struct{}) {
	klog.Infof("Starting CertRotation")
	defer klog.Infof("Shutting down CertRotation")
	c.WaitForReady(stopCh)

	go wait.Until(c.runServiceHostnames, time.Second, stopCh)
	go wait.Until(c.runExternalLoadBalancerHostnames, time.Second, stopCh)
	go wait.Until(c.runInternalLoadBalancerHostnames, time.Second, stopCh)

	for _, certRotator := range c.certRotators {
		go certRotator.Run(workers, stopCh)
	}
}

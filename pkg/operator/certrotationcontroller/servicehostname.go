package certrotationcontroller

import (
	"fmt"
	"net"

	"k8s.io/klog"

	"github.com/apparentlymart/go-cidr/cidr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
)

const workQueueKey = "key"

func (c *CertRotationController) syncServiceHostnames() error {
	hostnames := sets.NewString("kubernetes", "kubernetes.default", "kubernetes.default.svc")
	hostnames.Insert("openshift", "openshift.default", "openshift.default.svc")
	// our DNS operator doesn't allow this to change and doesn't expose this value anywhere.
	hostnames.Insert("kubernetes.default.svc." + "cluster.local")
	hostnames.Insert("openshift.default.svc." + "cluster.local")

	networkConfig, err := c.networkLister.Get("cluster")
	if err != nil {
		return err
	}
	for _, cidrString := range networkConfig.Status.ServiceNetwork {
		_, serviceCIDR, err := net.ParseCIDR(cidrString)
		if err != nil {
			return err
		}
		ip, err := cidr.Host(serviceCIDR, 1)
		if err != nil {
			return err
		}
		hostnames.Insert(ip.String())
	}

	klog.V(2).Infof("syncing servicenetwork hostnames: %v", hostnames.List())
	c.serviceNetwork.setHostnames(hostnames.List())
	return nil
}

func (c *CertRotationController) runServiceHostnames() {
	for c.processServiceHostnames() {
	}
}

func (c *CertRotationController) processServiceHostnames() bool {
	dsKey, quit := c.serviceHostnamesQueue.Get()
	if quit {
		return false
	}
	defer c.serviceHostnamesQueue.Done(dsKey)

	err := c.syncServiceHostnames()
	if err == nil {
		c.serviceHostnamesQueue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.serviceHostnamesQueue.AddRateLimited(dsKey)

	return true
}

// eventHandler queues the operator to check spec and status
func (c *CertRotationController) serviceHostnameEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.serviceHostnamesQueue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.serviceHostnamesQueue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.serviceHostnamesQueue.Add(workQueueKey) },
	}
}

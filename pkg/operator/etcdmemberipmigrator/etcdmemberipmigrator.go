package etcdmemberipmigrator

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"github.com/davecgh/go-spew/spew"
	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey = "key"
)

// watches etcd members and if they do not have a peer URL that has an IP address, the ip address is determined
// and added as the first address.
type EtcdMemberIPMigrator struct {
	operatorClient       v1helpers.OperatorClient
	etcdClient           etcdcli.EtcdClient
	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister

	cachesToSync  []cache.InformerSynced
	queue         workqueue.RateLimitingInterface
	eventRecorder events.Recorder
}

func NewEtcdMemberIPMigrator(
	operatorClient v1helpers.OperatorClient,
	kubeInformers informers.SharedInformerFactory,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	etcdClient etcdcli.EtcdClient,
	eventRecorder events.Recorder,
) *EtcdMemberIPMigrator {
	c := &EtcdMemberIPMigrator{
		operatorClient:       operatorClient,
		etcdClient:           etcdClient,
		nodeLister:           kubeInformers.Core().V1().Nodes().Lister(),
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),

		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			kubeInformers.Core().V1().Nodes().Informer().HasSynced,
			infrastructureInformer.Informer().HasSynced,
			networkInformer.Informer().HasSynced,
			operatorClient.Informer().HasSynced,
		},
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EtcdMemberIPMigrator"),
		eventRecorder: eventRecorder.WithComponentSuffix("etcd-member-ip-migrator"),
	}
	kubeInformers.Core().V1().Nodes().Informer().AddEventHandler(c.eventHandler())
	networkInformer.Informer().AddEventHandler(c.eventHandler())
	operatorClient.Informer().AddEventHandler(c.eventHandler())

	return c
}

func (c *EtcdMemberIPMigrator) sync() error {
	err := c.reconcileMembers()
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMemberIPMigratorDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("EtcdMemberIPMigratorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "EtcdMemberIPMigratorDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *EtcdMemberIPMigrator) reconcileMembers() error {
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers()
	if err != nil {
		return err
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("unhealthy members: %v", spew.Sdump(unhealthyMembers))
		memberNames := []string{}
		for _, member := range unhealthyMembers {
			memberNames = append(memberNames, member.Name)
		}
		c.eventRecorder.Eventf("UnhealthyEtcdMember", "unhealthy members: %v", strings.Join(memberNames, ","))
		return nil
	}

	members, err := c.etcdClient.MemberList()
	if err != nil {
		return err
	}
	for _, member := range members {
		// take no action on members that have not yet joined
		if len(member.Name) == 0 {
			continue
		}
		if member.IsLearner {
			continue
		}
		hasPeerIP, err := hasPeerIP(member.PeerURLs)
		if err != nil {
			return err
		}
		if hasPeerIP {
			continue
		}

		c.eventRecorder.Eventf("MemberMissingIPPeer", "member %q is missing an IP in the peer list", member.Name)

		// if the member does not have a peerIP, then we need to migrate it.  This approach is very heavy with dns requests
		// but this should almost never happen and this allows us to re-use the code that we have already for DNS.
		etcdInfo, err := c.getEtcdInfo()
		if err != nil {
			return err
		}
		requiredPeerList, err := getRequiredPeerList(etcdInfo, member.PeerURLs)
		if err != nil {
			return err
		}
		c.eventRecorder.Eventf("MemberSettingIPPeer", "member %q; new peer list %v", member.Name, strings.Join(requiredPeerList, ","))
		if err := c.etcdClient.MemberUpdatePeerURL(member.ID, requiredPeerList); err != nil {
			return err
		}
	}

	return nil
}

func hasPeerIP(peerURLs []string) (bool, error) {
	errors := []error{}

	for _, currPeerURL := range peerURLs {
		peerURL, err := url.Parse(currPeerURL)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		host, _, err := net.SplitHostPort(peerURL.Host)
		if err != nil {
			errors = append(errors, err)
			continue
		}
		peerIP := net.ParseIP(host)
		if peerIP != nil {
			return true, nil
		}
	}

	if len(errors) == 0 {
		return false, nil
	}

	return false, utilerrors.NewAggregate(errors)
}

func getRequiredPeerList(etcdInfos []etcdInfo, existingPeerURLs []string) ([]string, error) {
	var needle *etcdInfo
	for _, currPeerURL := range existingPeerURLs {
		for i := range etcdInfos {
			currEtcdInfo := etcdInfos[i]
			if len(currEtcdInfo.nodeDNSName) == 0 {
				continue
			}
			if strings.Contains(currPeerURL, currEtcdInfo.nodeDNSName) {
				needle = &currEtcdInfo
			}
		}
		if needle != nil {
			break
		}
	}
	if needle == nil {
		dnsErrors := []error{}
		for _, currEtcdInfo := range etcdInfos {
			if currEtcdInfo.nodeDNSErr != nil {
				dnsErrors = append(dnsErrors, currEtcdInfo.nodeDNSErr)
			}
		}
		return nil, fmt.Errorf("unable to locate a node for peerURL=%v, dnsErrors=%v", strings.Join(existingPeerURLs, ","), utilerrors.NewAggregate(dnsErrors))
	}

	// append instead of prepend so the discover-initial-etcd-cluster command still matches the pod.
	requiredPeerList := append([]string{}, existingPeerURLs...)
	requiredPeerList = append(requiredPeerList, fmt.Sprintf("https://%s:2380", needle.preferredNodeIPForURL))
	return requiredPeerList, nil
}

type etcdInfo struct {
	nodeName              string
	preferredNodeIPForURL string
	// nodeDNSName may be empty in some cases where we never supported DNS. This is ok and is only an error if
	// we cannot find a match in a later step
	nodeDNSName string
	// nodeDNSErr is filled in if we cannot find a dns name
	nodeDNSErr error
}

func (c *EtcdMemberIPMigrator) getEtcdInfo() ([]etcdInfo, error) {
	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return nil, err
	}
	etcdDiscoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return nil, err
	}

	ret := []etcdInfo{}
	for _, node := range nodes {
		internalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}

		nodeDNSName, nodeDNSErr := dnshelpers.ReverseLookupFirstHit(etcdDiscoveryDomain, internalIP)
		currEtcdInfo := etcdInfo{
			nodeName:              node.Name,
			preferredNodeIPForURL: internalIP,
			nodeDNSName:           nodeDNSName,
			nodeDNSErr:            nodeDNSErr,
		}
		ret = append(ret, currEtcdInfo)
	}

	return ret, nil
}

func (c *EtcdMemberIPMigrator) getEtcdDiscoveryDomain() (string, error) {
	infrastructure, err := c.infrastructureLister.Get("cluster")
	if err != nil {
		return "", err
	}
	etcdDiscoveryDomain := infrastructure.Status.EtcdDiscoveryDomain
	if len(etcdDiscoveryDomain) == 0 {
		return "", fmt.Errorf("infrastructures.config.openshit.io/cluster missing .status.etcdDiscoveryDomain")
	}
	return etcdDiscoveryDomain, nil
}

func (c *EtcdMemberIPMigrator) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}

func (c *EtcdMemberIPMigrator) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting EtcdMemberIPMigrator")
	defer klog.Infof("Shutting down EtcdMemberIPMigrator")

	if !cache.WaitForCacheSync(stopCh, c.cachesToSync...) {
		utilruntime.HandleError(fmt.Errorf("caches did not sync"))
		return
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	go wait.Until(func() {
		c.queue.Add(workQueueKey)
	}, time.Minute, stopCh)

	<-stopCh
}

func (c *EtcdMemberIPMigrator) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *EtcdMemberIPMigrator) processNextWorkItem() bool {
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

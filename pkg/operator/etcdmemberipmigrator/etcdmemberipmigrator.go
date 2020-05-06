package etcdmemberipmigrator

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"

	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/informers"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
)

// watches etcd members and if they do not have a peer URL that has an IP address, the ip address is determined
// and added as the first address.
type EtcdMemberIPMigrator struct {
	operatorClient       v1helpers.OperatorClient
	etcdClient           etcdcli.EtcdCluster
	nodeLister           corev1listers.NodeLister
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
}

func NewEtcdMemberIPMigrator(
	operatorClient v1helpers.OperatorClient,
	kubeInformers informers.SharedInformerFactory,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
	etcdClient etcdcli.EtcdCluster,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &EtcdMemberIPMigrator{
		operatorClient:       operatorClient,
		etcdClient:           etcdClient,
		nodeLister:           kubeInformers.Core().V1().Nodes().Lister(),
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),
	}

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		operatorClient.Informer(),
		kubeInformers.Core().V1().Nodes().Informer(),
		infrastructureInformer.Informer(),
		networkInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ToController("EtcdMemberIPMigrator", eventRecorder.WithComponentSuffix("etcd-member-ip-migrator"))
}

func (c *EtcdMemberIPMigrator) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.reconcileMembers(syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdMemberIPMigratorDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("EtcdMemberIPMigratorUpdatingStatus", updateErr.Error())
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

func (c *EtcdMemberIPMigrator) reconcileMembers(recorder events.Recorder) error {
	unhealthyMembers, err := c.etcdClient.UnhealthyMembers()
	if err != nil {
		return err
	}
	if len(unhealthyMembers) > 0 {
		klog.V(4).Infof("unhealthy members: %v", spew.Sdump(unhealthyMembers))
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

		recorder.Eventf("MemberMissingIPPeer", "member %q is missing an IP in the peer list", member.Name)

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
		recorder.Eventf("MemberSettingIPPeer", "member %q; new peer list %v", member.Name, strings.Join(requiredPeerList, ","))
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

	return []string{fmt.Sprintf("https://%s:2380", needle.preferredNodeIPForURL)}, nil
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

		nodeDNSName, nodeDNSErr := reverseLookupFirstHit(etcdDiscoveryDomain, internalIP)
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

// this is the only DNS call anywhere.  Don't copy/paste this or use it somewhere else.  We want to remove this in 4.5
func reverseLookupFirstHit(discoveryDomain string, ips ...string) (string, error) {
	errs := []error{}
	for _, ip := range ips {
		ret, err := reverseLookupForOneIP(discoveryDomain, ip)
		if err == nil {
			return ret, nil
		}
		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return "", fmt.Errorf("something weird happened for %q, %#v", discoveryDomain, ips)
	}
	return "", utilerrors.NewAggregate(errs)
}

func reverseLookupForOneIP(discoveryDomain, ipAddress string) (string, error) {
	service := "etcd-server-ssl"
	proto := "tcp"

	_, srvs, err := net.LookupSRV(service, proto, discoveryDomain)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == ipAddress {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}

package etcdcli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	deadlock "github.com/sasha-s/go-deadlock"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/api/v3rpc/rpctypes"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

const BootstrapIPAnnotationKey = "alpha.installer.openshift.io/etcd-bootstrap"

type etcdClientGetter struct {
	nodeLister      corev1listers.NodeLister
	endpointsLister corev1listers.EndpointsLister
	networkLister   configv1listers.NetworkLister

	nodeListerSynced      cache.InformerSynced
	endpointsListerSynced cache.InformerSynced
	networkListerSynced   cache.InformerSynced

	eventRecorder events.Recorder

	clientLock          deadlock.Mutex
	lastClientConfigKey []string
	cachedClient        *clientv3.Client
}

func NewEtcdClient(kubeInformers v1helpers.KubeInformersForNamespaces, networkInformer configv1informers.NetworkInformer, eventRecorder events.Recorder) EtcdClient {
	return &etcdClientGetter{
		nodeLister:            kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		endpointsLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		networkLister:         networkInformer.Lister(),
		nodeListerSynced:      kubeInformers.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		endpointsListerSynced: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
		networkListerSynced:   networkInformer.Informer().HasSynced,
		eventRecorder:         eventRecorder.WithComponentSuffix("etcd-client"),
	}
}

// getEtcdClient may return a cached client.  When a new client is needed, the previous client is closed.
// The caller should not closer the client or future calls may fail.
func (g *etcdClientGetter) getEtcdClient() (*clientv3.Client, error) {
	if !g.nodeListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}
	if !g.endpointsListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}
	if !g.networkListerSynced() {
		return nil, fmt.Errorf("network lister not synced")
	}

	network, err := g.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	etcdEndpoints := []string{}
	nodes, err := g.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	for _, node := range nodes {
		internalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", internalIP))
	}

	hostEtcd, err := g.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd-2")
	if err != nil {
		return nil, err
	}
	bootstrapIP, ok := hostEtcd.Annotations[BootstrapIPAnnotationKey]
	if !ok {
		klog.V(2).Infof("service/host-etcd-2 is missing annotation %s", BootstrapIPAnnotationKey)
	}
	if bootstrapIP != "" {
		// escape if IPv6
		if net.ParseIP(bootstrapIP).To4() == nil {
			bootstrapIP = "[" + bootstrapIP + "]"
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", bootstrapIP))
	}

	g.clientLock.Lock()
	defer g.clientLock.Unlock()
	// TODO check if the connection is already closed
	if reflect.DeepEqual(g.lastClientConfigKey, etcdEndpoints) {
		return g.cachedClient, nil
	}

	c, err := getEtcdClient(etcdEndpoints)
	if err != nil {
		return nil, err
	}
	if g.cachedClient != nil {
		if err := g.cachedClient.Close(); err != nil {
			utilruntime.HandleError(err)
		}
	}
	g.cachedClient = c
	g.lastClientConfigKey = etcdEndpoints

	return g.cachedClient, nil
}

func getEtcdClient(endpoints []string) (*clientv3.Client, error) {
	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
	}

	tlsInfo := transport.TLSInfo{
		CertFile:      "/var/run/secrets/etcd-client/tls.crt",
		KeyFile:       "/var/run/secrets/etcd-client/tls.key",
		TrustedCAFile: "/var/run/configmaps/etcd-ca/ca-bundle.crt",
	}
	tlsConfig, err := tlsInfo.ClientConfig()

	cfg := &clientv3.Config{
		DialOptions: dialOptions,
		Endpoints:   endpoints,
		DialTimeout: 15 * time.Second,
		TLS:         tlsConfig,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return cli, err
}

func (g *etcdClientGetter) MemberAdd(peerURL string) error {
	g.eventRecorder.Eventf("MemberAdd", "adding new peer %v", peerURL)

	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return err
	}

	for _, member := range membersResp.Members {
		for _, currPeerURL := range member.PeerURLs {
			if currPeerURL == peerURL {
				g.eventRecorder.Warningf("MemberAlreadyAdded", "member with peerURL %s already part of the cluster", peerURL)
				return nil
			}
		}
	}

	_, err = cli.MemberAdd(ctx, []string{peerURL})
	if err != nil {
		return err
	}
	return err
}

func (g *etcdClientGetter) MemberUpdatePeerURL(id uint64, peerURLs []string) error {
	if members, err := g.MemberList(); err != nil {
		g.eventRecorder.Eventf("MemberUpdate", "updating member %d with peers %v", id, strings.Join(peerURLs, ","))
	} else {
		memberName := fmt.Sprintf("%d", id)
		for _, member := range members {
			if member.ID == id {
				memberName = member.Name
				break
			}
		}
		g.eventRecorder.Eventf("MemberUpdate", "updating member %q with peers %v", memberName, strings.Join(peerURLs, ","))
	}

	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = cli.MemberUpdate(ctx, id, peerURLs)
	if err != nil {
		return err
	}
	return err
}

func (g *etcdClientGetter) MemberRemove(member string) error {
	g.eventRecorder.Eventf("MemberRemove", "removing member %q", member)

	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return nil
	}

	for _, m := range membersResp.Members {
		if m.Name == member {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			_, err = cli.MemberRemove(ctx, m.ID)
			if err != nil {
				return err
			}
			return nil
		}
	}

	g.eventRecorder.Warningf("MemberAlreadyRemoved", "member %q already removed", member)
	return nil
}

func (g *etcdClientGetter) MemberList() ([]*etcdserverpb.Member, error) {
	cli, err := g.getEtcdClient()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	return membersResp.Members, nil
}

func (g *etcdClientGetter) GetMember(name string) (*etcdserverpb.Member, error) {
	members, err := g.MemberList()
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if m.Name == name {
			return m, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "etcd.operator.openshift.io", Resource: "etcdmembers"}, name)
}

// GetMemberNameOrHost If the member's name is not set, extract ip/hostname from peerURL. Useful with unstarted members.
func GetMemberNameOrHost(member *etcdserverpb.Member) string {
	if len(member.Name) == 0 {
		u, err := url.Parse(member.PeerURLs[0])
		if err != nil {
			klog.Errorf("unstarted member has invalid peerURL: %#v", err)
			return "NAME-PENDING-BAD-PEER-URL"
		}
		return fmt.Sprintf("NAME-PENDING-%s", u.Hostname())
	}
	return member.Name
}

func (g *etcdClientGetter) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	cli, err := g.getEtcdClient()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	etcd, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	etcdMemberHealthCheck, err := g.getMemberHealth(etcd.Members)
	if err != nil {
		return nil, err
	}

	_, unhealthyMemberNames, unstartedMemberNames := etcdMemberHealthCheck.Status()

	if len(unstartedMemberNames) > 0 {
		g.eventRecorder.Warningf("UnstartedEtcdMember", "unstarted members: %v", strings.Join(unstartedMemberNames, ","))
	}

	if len(unhealthyMemberNames) > 0 {
		g.eventRecorder.Warningf("UnhealthyEtcdMember", "unhealthy members: %v", strings.Join(unhealthyMemberNames, ","))
	}

	_, unhealthyMembers := etcdMemberHealthCheck.MemberStatus()

	return unhealthyMembers, nil
}

func (g *etcdClientGetter) MemberHealth(etcdMembers []*etcdserverpb.Member) (*memberHealth, error) {
	memberHealth, err := g.getMemberHealth(etcdMembers)
	if err != nil {
		return nil, err
	}
	return memberHealth, nil
}

func (g *etcdClientGetter) getMemberHealth(etcdMembers []*etcdserverpb.Member) (*memberHealth, error) {
	var wg sync.WaitGroup
	memberHealth := &memberHealth{}
	hch := make(chan healthCheck, len(etcdMembers))
	for _, member := range etcdMembers {
		if len(member.ClientURLs) == 0 {
			memberHealth.Check = append(memberHealth.Check, &healthCheck{Member: member, Health: false, Started: false})
			continue
		}
		wg.Add(1)
		go func(member *etcdserverpb.Member) {
			defer wg.Done()
			cli, err := getEtcdClient([]string{member.ClientURLs[0]})
			if err != nil {
				hch <- healthCheck{Member: member, Health: false, Started: true, Error: err.Error()}
				return
			}
			defer cli.Close()
			st := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			_, err = cli.Get(ctx, "health")
			cancel()
			hc := healthCheck{Member: member, Health: false, Started: true, Took: time.Since(st).String()}
			if err == nil || err == rpctypes.ErrPermissionDenied {
				hc.Health = true
			} else {
				hc.Error = err.Error()
			}
			hch <- hc
		}(member)
	}

	go func() {
		wg.Wait()
		close(hch)
	}()

	for healthCheck := range hch {
		memberHealth.Check = append(memberHealth.Check, &healthCheck)
	}

	return memberHealth, nil
}

func (g *etcdClientGetter) MemberStatus(member *etcdserverpb.Member) string {
	cli, err := g.getEtcdClient()
	if err != nil {
		klog.Errorf("error getting etcd client: %#v", err)
		return EtcdMemberStatusUnknown
	}

	if len(member.ClientURLs) == 0 && member.Name == "" {
		return EtcdMemberStatusNotStarted
	}

	ctx, cancel := context.WithCancel(context.Background())
	_, err = cli.Status(ctx, member.ClientURLs[0])
	cancel()
	if err != nil {
		klog.Errorf("error getting etcd member %s status: %#v", member.Name, err)
		return EtcdMemberStatusUnhealthy
	}

	return EtcdMemberStatusAvailable
}

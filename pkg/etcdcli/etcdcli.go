package etcdcli

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/pkg/v3/logutil"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	BootstrapIPAnnotationKey    = "alpha.installer.openshift.io/etcd-bootstrap"
	DefaultDialTimeout          = 15 * time.Second
	DefaultDialKeepAliveTimeout = 60 * time.Second
	DefragDialTimeout           = 60 * time.Second
	DefaultClientTimeout        = 30 * time.Second
)

type etcdClientGetter struct {
	nodeLister       corev1listers.NodeLister
	configmapsLister corev1listers.ConfigMapLister
	networkLister    configv1listers.NetworkLister

	nodeListerSynced       cache.InformerSynced
	configmapsListerSynced cache.InformerSynced
	networkListerSynced    cache.InformerSynced

	eventRecorder events.Recorder

	clientPool *EtcdClientPool
}

func NewEtcdClient(kubeInformers v1helpers.KubeInformersForNamespaces, networkInformer configv1informers.NetworkInformer, eventRecorder events.Recorder) EtcdClient {
	g := &etcdClientGetter{
		nodeLister:             kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		configmapsLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Lister(),
		networkLister:          networkInformer.Lister(),
		nodeListerSynced:       kubeInformers.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		configmapsListerSynced: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps().Informer().HasSynced,
		networkListerSynced:    networkInformer.Informer().HasSynced,
		eventRecorder:          eventRecorder.WithComponentSuffix("etcd-client"),
	}

	endpointFunc := func() ([]string, error) {
		return endpoints(g.nodeLister, g.configmapsLister, g.networkLister,
			g.nodeListerSynced, g.configmapsListerSynced, g.networkListerSynced)
	}
	newFunc := func() (*clientv3.Client, error) {
		endpoints, err := endpointFunc()
		if err != nil {
			return nil, fmt.Errorf("error retrieving endpoints for new cached client: %w", err)
		}
		return newEtcdClientWithClientOpts(endpoints, true)
	}

	g.clientPool = NewDefaultEtcdClientPool(newFunc, endpointFunc)
	return g
}

// newEtcdClientWithClientOpts allows customization of the etcd client using ClientOptions. All clients must be manually
// closed by the caller with Close().
func newEtcdClientWithClientOpts(endpoints []string, skipConnectionTest bool, opts ...ClientOption) (*clientv3.Client, error) {
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(ioutil.Discard, ioutil.Discard, os.Stderr))
	clientOpts, err := newClientOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("error during clientOpts: %w", err)
	}

	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
		grpc.WithChainUnaryInterceptor(grpcprom.UnaryClientInterceptor),
		grpc.WithChainStreamInterceptor(grpcprom.StreamClientInterceptor),
	}

	tlsInfo := transport.TLSInfo{
		CertFile:      "/var/run/secrets/etcd-client/tls.crt",
		KeyFile:       "/var/run/secrets/etcd-client/tls.key",
		TrustedCAFile: "/var/run/configmaps/etcd-ca/ca-bundle.crt",
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("error during client TLSConfig: %w", err)
	}

	// Our logs are noisy
	lcfg := logutil.DefaultZapLoggerConfig
	lcfg.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	l, err := lcfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed building client logger: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultClientTimeout)
	defer cancel()

	cfg := &clientv3.Config{
		Context:              ctx,
		DialOptions:          dialOptions,
		Endpoints:            endpoints,
		DialTimeout:          clientOpts.dialTimeout,
		DialKeepAliveTimeout: clientOpts.dialKeepAliveTimeout,
		TLS:                  tlsConfig,
		Logger:               l,
	}

	// Note that this already establishes a connection that can fail, whereas skipConnectionTest only applies to the member listing,
	// which will fail when the member would be a learner
	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to make etcd client for endpoints %v: %w", endpoints, err)
	}

	// If the endpoint includes a learner member then we skip the test
	// as learner members don't support member list
	if skipConnectionTest {
		return cli, err
	}
	_, err = cli.MemberList(ctx)
	if err != nil {
		if clientv3.IsConnCanceled(err) {
			return nil, fmt.Errorf("client connection was canceled: %w", err)
		}
		return nil, fmt.Errorf("error during client connection check: %w", err)
	}

	return cli, err
}

func (g *etcdClientGetter) MemberAddAsLearner(ctx context.Context, peerURL string) error {
	cli, err := g.clientPool.Get()
	if err != nil {
		return err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
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

	defer func() {
		if err != nil {
			g.eventRecorder.Warningf("MemberAddAsLearner", "failed to add new member %s: %v", peerURL, err)
		} else {
			g.eventRecorder.Eventf("MemberAddAsLearner", "successfully added new member %s", peerURL)
		}
	}()

	_, err = cli.MemberAddAsLearner(ctx, []string{peerURL})
	return err
}

func (g *etcdClientGetter) MemberPromote(ctx context.Context, member *etcdserverpb.Member) error {
	cli, err := g.clientPool.Get()
	if err != nil {
		return err
	}

	defer g.clientPool.Return(cli)

	defer func() {
		if err != nil {
			// Not being ready for promotion can be a common event until the learner's log
			// catches up with the leader, so we don't emit events for failing for that case
			if err.Error() == etcdserver.ErrLearnerNotReady.Error() {
				return
			}
			g.eventRecorder.Warningf("MemberPromote", "failed to promote learner member %s: %v", member.PeerURLs[0], err)
		} else {
			g.eventRecorder.Eventf("MemberPromote", "successfully promoted learner member %v", member.PeerURLs[0])
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	_, err = cli.MemberPromote(ctx, member.ID)
	return err
}

func (g *etcdClientGetter) MemberUpdatePeerURL(ctx context.Context, id uint64, peerURLs []string) error {
	if members, err := g.MemberList(ctx); err != nil {
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

	cli, err := g.clientPool.Get()
	if err != nil {
		return err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	_, err = cli.MemberUpdate(ctx, id, peerURLs)
	if err != nil {
		return err
	}
	return err
}

func (g *etcdClientGetter) MemberRemove(ctx context.Context, memberID uint64) error {
	cli, err := g.clientPool.Get()
	if err != nil {
		return err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	_, err = cli.MemberRemove(ctx, memberID)
	if err == nil {
		g.eventRecorder.Eventf("MemberRemove", "removed member with ID: %v", memberID)
	}
	return err
}

func (g *etcdClientGetter) MemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	return membersResp.Members, nil
}

func (g *etcdClientGetter) VotingMemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	members, err := g.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	return filterVotingMembers(members), nil
}

// Status reports etcd endpoint status of client URL target. Example https://10.0.10.1:2379
func (g *etcdClientGetter) Status(ctx context.Context, clientURL string) (*clientv3.StatusResponse, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)
	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	return cli.Status(ctx, clientURL)
}

func (g *etcdClientGetter) GetMember(ctx context.Context, name string) (*etcdserverpb.Member, error) {
	members, err := g.MemberList(ctx)
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

func (g *etcdClientGetter) UnhealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	etcdCluster, err := cli.MemberList(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get member list %v", err)
	}

	memberHealth := GetMemberHealth(ctx, etcdCluster.Members)

	unstartedMemberNames := GetUnstartedMemberNames(memberHealth)
	if len(unstartedMemberNames) > 0 {
		klog.Warningf("UnstartedEtcdMember found: %v", unstartedMemberNames)
	}

	unhealthyMemberNames := GetUnhealthyMemberNames(memberHealth)
	if len(unhealthyMemberNames) > 0 {
		klog.Warningf("UnhealthyEtcdMember found: %v", unhealthyMemberNames)
	}

	return memberHealth.GetUnhealthyMembers(), nil
}

func (g *etcdClientGetter) UnhealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	unhealthyMembers, err := g.UnhealthyMembers(ctx)
	if err != nil {
		return nil, err
	}
	return filterVotingMembers(unhealthyMembers), nil
}

// HealthyMembers performs health check of current members and returns a slice of healthy members and error
// if no healthy members found.
func (g *etcdClientGetter) HealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	etcdCluster, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	healthyMembers := GetMemberHealth(ctx, etcdCluster.Members).GetHealthyMembers()
	if len(healthyMembers) == 0 {
		return nil, fmt.Errorf("no healthy etcd members found")
	}

	return healthyMembers, nil
}

func (g *etcdClientGetter) HealthyVotingMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	healthyMembers, err := g.HealthyMembers(ctx)
	if err != nil {
		return nil, err
	}
	return filterVotingMembers(healthyMembers), nil
}

func (g *etcdClientGetter) MemberHealth(ctx context.Context) (memberHealth, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	etcdCluster, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	return GetMemberHealth(ctx, etcdCluster.Members), nil
}

func (g *etcdClientGetter) IsMemberHealthy(ctx context.Context, member *etcdserverpb.Member) (bool, error) {
	if member == nil {
		return false, fmt.Errorf("member can not be nil")
	}
	memberHealth := GetMemberHealth(ctx, []*etcdserverpb.Member{member})
	if len(memberHealth) == 0 {
		return false, fmt.Errorf("member health check failed")
	}
	if memberHealth[0].Healthy {
		return true, nil
	}

	return false, nil
}

func (g *etcdClientGetter) MemberStatus(ctx context.Context, member *etcdserverpb.Member) string {
	cli, err := g.clientPool.Get()
	if err != nil {
		klog.Errorf("error getting etcd client: %#v", err)
		return EtcdMemberStatusUnknown
	}
	defer g.clientPool.Return(cli)

	if len(member.ClientURLs) == 0 && member.Name == "" {
		return EtcdMemberStatusNotStarted
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultClientTimeout)
	defer cancel()
	_, err = cli.Status(ctx, member.ClientURLs[0])
	if err != nil {
		klog.Errorf("error getting etcd member %s status: %#v", member.Name, err)
		return EtcdMemberStatusUnhealthy
	}

	return EtcdMemberStatusAvailable
}

// Defragment creates a new uncached clientv3 to the given member url and calls clientv3.Client.Defragment.
func (g *etcdClientGetter) Defragment(ctx context.Context, member *etcdserverpb.Member) (*clientv3.DefragmentResponse, error) {
	// no g.clientLock necessary, this always returns a new fresh client
	cli, err := newEtcdClientWithClientOpts([]string{member.ClientURLs[0]}, false, WithDialTimeout(DefragDialTimeout))
	if err != nil {
		return nil, fmt.Errorf("failed to get etcd client for defragment: %w", err)
	}
	defer func() {
		if cli == nil {
			return
		}
		if err := cli.Close(); err != nil {
			klog.Errorf("error closing etcd client for defrag: %v", err)
		}
	}()

	resp, err := cli.Defragment(ctx, member.ClientURLs[0])
	if err != nil {
		return nil, fmt.Errorf("error while running defragment: %w", err)
	}
	return resp, nil
}

func endpoints(nodeLister corev1listers.NodeLister,
	configmapsLister corev1listers.ConfigMapLister,
	networkLister configv1listers.NetworkLister,
	nodeListerSynced cache.InformerSynced,
	configmapsListerSynced cache.InformerSynced,
	networkListerSynced cache.InformerSynced) ([]string, error) {

	if !nodeListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}
	if !configmapsListerSynced() {
		return nil, fmt.Errorf("configmaps lister not synced")
	}
	if !networkListerSynced() {
		return nil, fmt.Errorf("network lister not synced")
	}

	var etcdEndpoints []string
	// This configmap should always only contain voting members. Learners should not be present here.
	configmap, err := configmapsLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
	if err != nil {
		klog.Errorf("failed to list endpoints from %s/%s, falling back to listing nodes: %v",
			operatorclient.TargetNamespace, "etcd-endpoints", err)

		network, err := networkLister.Get("cluster")
		if err != nil {
			return nil, fmt.Errorf("failed to list cluster network: %w", err)
		}

		nodes, err := nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
		if err != nil {
			return nil, fmt.Errorf("failed to list control plane nodes: %w", err)
		}
		for _, node := range nodes {
			internalIP, _, err := dnshelpers.GetPreferredInternalIPAddressForNodeName(network, node)
			if err != nil {
				return nil, fmt.Errorf("failed to get internal IP for node: %w", err)
			}
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s", net.JoinHostPort(internalIP, "2379")))
		}
	} else {
		if bootstrapIP, ok := configmap.Annotations[BootstrapIPAnnotationKey]; ok && bootstrapIP != "" {
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s", net.JoinHostPort(bootstrapIP, "2379")))
		}
		for _, etcdEndpointIp := range configmap.Data {
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s", net.JoinHostPort(etcdEndpointIp, "2379")))
		}
	}

	if len(etcdEndpoints) == 0 {
		return nil, fmt.Errorf("endpoints func found no etcd endpoints")
	}

	sort.Strings(etcdEndpoints)
	return etcdEndpoints, nil
}

// filterVotingMembers filters out learner members
func filterVotingMembers(members []*etcdserverpb.Member) []*etcdserverpb.Member {
	var votingMembers []*etcdserverpb.Member
	for _, member := range members {
		if member.IsLearner {
			continue
		}
		votingMembers = append(votingMembers, member)
	}
	return votingMembers
}

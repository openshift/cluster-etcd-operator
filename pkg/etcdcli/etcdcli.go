package etcdcli

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
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
	BootstrapIPAnnotationKey = "alpha.installer.openshift.io/etcd-bootstrap"
	DefaultDialTimeout       = 15 * time.Second
	DefragDialTimeout        = 60 * time.Second
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
		if !g.nodeListerSynced() {
			return nil, fmt.Errorf("node lister not synced")
		}
		if !g.configmapsListerSynced() {
			return nil, fmt.Errorf("configmaps lister not synced")
		}
		if !g.networkListerSynced() {
			return nil, fmt.Errorf("network lister not synced")
		}

		network, err := g.networkLister.Get("cluster")
		if err != nil {
			return nil, fmt.Errorf("failed to list cluster network: %w", err)
		}

		var etcdEndpoints []string
		nodes, err := g.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
		if err != nil {
			return nil, fmt.Errorf("failed to list control plane nodes: %w", err)
		}
		for _, node := range nodes {
			internalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
			if err != nil {
				return nil, fmt.Errorf("failed to get internal IP for node: %w", err)
			}
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", internalIP))
		}

		configmap, err := g.configmapsLister.ConfigMaps(operatorclient.TargetNamespace).Get("etcd-endpoints")
		if err != nil {
			return nil, fmt.Errorf("failed to list endpoints from %s/%s: %w",
				operatorclient.TargetNamespace, "etcd-endpoints", err)
		}
		if bootstrapIP, ok := configmap.Annotations[BootstrapIPAnnotationKey]; ok && bootstrapIP != "" {
			// escape if IPv6
			if net.ParseIP(bootstrapIP).To4() == nil {
				bootstrapIP = "[" + bootstrapIP + "]"
			}
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", bootstrapIP))
		}

		return etcdEndpoints, nil
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

	cfg := &clientv3.Config{
		DialOptions: dialOptions,
		Endpoints:   endpoints,
		DialTimeout: clientOpts.dialTimeout,
		TLS:         tlsConfig,
		Logger:      l,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to make etcd client for endpoints %v: %w", endpoints, err)
	}

	// If the endpoint includes a learner member then we skip the test
	// as learner members don't support member list
	if skipConnectionTest {
		return cli, err
	}

	// Test client connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = cli.MemberList(ctx)
	if err != nil {
		if clientv3.IsConnCanceled(err) {
			return nil, fmt.Errorf("client connection was canceled: %w", err)
		}
		return nil, fmt.Errorf("error during client connection check: %w", err)
	}

	return cli, err
}

func (g *etcdClientGetter) MemberAdd(peerURL string) error {
	g.eventRecorder.Eventf("MemberAdd", "adding new peer %v", peerURL)

	cli, err := g.clientPool.Get()
	if err != nil {
		return err
	}
	defer g.clientPool.Return(cli)

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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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

	_, err = cli.MemberUpdate(ctx, id, peerURLs)
	if err != nil {
		return err
	}
	return err
}

func (g *etcdClientGetter) MemberRemove(member string) error {
	g.eventRecorder.Eventf("MemberRemove", "removing member %q", member)

	cli, err := g.clientPool.Get()
	if err != nil {
		return err
	}
	defer g.clientPool.Return(cli)

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

func (g *etcdClientGetter) MemberList(ctx context.Context) ([]*etcdserverpb.Member, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	return membersResp.Members, nil
}

// Status reports etcd endpoint status of client URL target. Example https://10.0.10.1:2379
func (g *etcdClientGetter) Status(ctx context.Context, clientURL string) (*clientv3.StatusResponse, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)
	return cli.Status(ctx, clientURL)
}

func (g *etcdClientGetter) GetMember(name string) (*etcdserverpb.Member, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

func (g *etcdClientGetter) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	etcdCluster, err := cli.MemberList(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get member list %v", err)
	}

	memberHealth := getMemberHealth(etcdCluster.Members)

	unstartedMemberNames := GetUnstartedMemberNames(memberHealth)
	if len(unstartedMemberNames) > 0 {
		g.eventRecorder.Warningf("UnstartedEtcdMember", "unstarted members: %v", strings.Join(unstartedMemberNames, ","))
	}

	unhealthyMemberNames := GetUnhealthyMemberNames(memberHealth)
	if len(unhealthyMemberNames) > 0 {
		g.eventRecorder.Warningf("UnhealthyEtcdMember", "unhealthy members: %v", strings.Join(unhealthyMemberNames, ","))
	}

	return memberHealth.GetUnhealthyMembers(), nil
}

// HealthyMembers performs health check of current members and returns a slice of healthy members and error
// if no healthy members found.
func (g *etcdClientGetter) HealthyMembers(ctx context.Context) ([]*etcdserverpb.Member, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	etcdCluster, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	healthyMembers := getMemberHealth(etcdCluster.Members).GetHealthyMembers()
	if len(healthyMembers) == 0 {
		return nil, fmt.Errorf("no healthy etcd members found")
	}

	return healthyMembers, nil
}

func (g *etcdClientGetter) MemberHealth(ctx context.Context) (memberHealth, error) {
	cli, err := g.clientPool.Get()
	if err != nil {
		return nil, err
	}

	defer g.clientPool.Return(cli)

	etcdCluster, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	return getMemberHealth(etcdCluster.Members), nil
}

func (g *etcdClientGetter) IsMemberHealthy(member *etcdserverpb.Member) (bool, error) {
	if member == nil {
		return false, fmt.Errorf("member can not be nil")
	}
	memberHealth := getMemberHealth([]*etcdserverpb.Member{member})
	if len(memberHealth) == 0 {
		return false, fmt.Errorf("member health check failed")
	}
	if memberHealth[0].Healthy {
		return true, nil
	}

	return false, nil
}

func (g *etcdClientGetter) MemberStatus(member *etcdserverpb.Member) string {
	cli, err := g.clientPool.Get()
	if err != nil {
		klog.Errorf("error getting etcd client: %#v", err)
		return EtcdMemberStatusUnknown
	}
	defer g.clientPool.Return(cli)

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

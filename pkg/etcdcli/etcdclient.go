package etcdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

var _ EtcdClient = &cachingEtcdClient{}

const BootstrapIPAnnotationKey = "alpha.installer.openshift.io/etcd-bootstrap"

type cachingEtcdClient struct {
	nodeLister      corev1listers.NodeLister
	endpointsLister corev1listers.EndpointsLister
	networkLister   configv1listers.NetworkLister

	nodeListerSynced      cache.InformerSynced
	endpointsListerSynced cache.InformerSynced
	networkListerSynced   cache.InformerSynced

	clientLock          sync.Mutex
	client              *clientv3.Client
	lastClientConfigKey []string
}

// NewCachingEtcdClient returns a new etcd client which refreshes in response to endpoint changes.
func NewCachingEtcdClient(kubeInformers v1helpers.KubeInformersForNamespaces, networkInformer configv1informers.NetworkInformer, eventRecorder events.Recorder) EtcdClient {
	return &cachingEtcdClient{
		nodeLister:            kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		endpointsLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		networkLister:         networkInformer.Lister(),
		nodeListerSynced:      kubeInformers.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		endpointsListerSynced: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
		networkListerSynced:   networkInformer.Informer().HasSynced,
	}
}

// getEtcdClient may return a cached client.  When a new client is needed, the previous client is closed.
// The caller should not closer the client or future calls may fail.
func (c *cachingEtcdClient) getEtcdClient() (*clientv3.Client, error) {
	c.clientLock.Lock()
	defer c.clientLock.Unlock()

	// TODO: inject done channel from context
	if !cache.WaitForCacheSync(make(<-chan struct{}), c.nodeListerSynced, c.endpointsListerSynced, c.networkListerSynced) {
		return nil, errors.New("timed out waiting for informer caches to sync")
	}

	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	etcdEndpoints := []string{}
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	for _, node := range nodes {
		internalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", internalIP))
	}

	hostEtcd, err := c.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd-2")
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

	// check if the connection is already closed
	if reflect.DeepEqual(c.lastClientConfigKey, etcdEndpoints) &&
		c.client.ActiveConnection().GetState() == connectivity.Ready {
		return c.client, nil
	}

	newClient, err := c.newEtcdClient(etcdEndpoints)
	if err != nil {
		return nil, err
	}
	if c.client != nil {
		if err := c.client.Close(); err != nil {
			utilruntime.HandleError(err)
		}
	}
	c.client = newClient
	c.lastClientConfigKey = etcdEndpoints

	klog.V(0).Infof("recreated etcd client")
	return c.client, nil
}

func (c *cachingEtcdClient) newEtcdClient(endpoints []string) (*clientv3.Client, error) {
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

func (c *cachingEtcdClient) MemberList(ctx context.Context) (*clientv3.MemberListResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MemberList(ctx)
	}
}

func (c *cachingEtcdClient) MemberAdd(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MemberAdd(ctx, peerAddrs)
	}
}

func (c *cachingEtcdClient) MemberAddAsLearner(ctx context.Context, peerAddrs []string) (*clientv3.MemberAddResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MemberAddAsLearner(ctx, peerAddrs)
	}
}

func (c *cachingEtcdClient) MemberRemove(ctx context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MemberRemove(ctx, id)
	}
}

func (c *cachingEtcdClient) MemberUpdate(ctx context.Context, id uint64, peerAddrs []string) (*clientv3.MemberUpdateResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MemberUpdate(ctx, id, peerAddrs)
	}
}

func (c *cachingEtcdClient) MemberPromote(ctx context.Context, id uint64) (*clientv3.MemberPromoteResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MemberPromote(ctx, id)
	}
}

func (c *cachingEtcdClient) AlarmList(ctx context.Context) (*clientv3.AlarmResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.AlarmList(ctx)
	}
}

func (c *cachingEtcdClient) AlarmDisarm(ctx context.Context, m *clientv3.AlarmMember) (*clientv3.AlarmResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.AlarmDisarm(ctx, m)
	}
}

func (c *cachingEtcdClient) Defragment(ctx context.Context, endpoint string) (*clientv3.DefragmentResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.Defragment(ctx, endpoint)
	}
}

func (c *cachingEtcdClient) Status(ctx context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.Status(ctx, endpoint)
	}
}

func (c *cachingEtcdClient) HashKV(ctx context.Context, endpoint string, rev int64) (*clientv3.HashKVResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.HashKV(ctx, endpoint, rev)
	}
}

func (c *cachingEtcdClient) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.Snapshot(ctx)
	}
}

func (c *cachingEtcdClient) MoveLeader(ctx context.Context, transfereeID uint64) (*clientv3.MoveLeaderResponse, error) {
	if client, err := c.getEtcdClient(); err != nil {
		return nil, err
	} else {
		return client.MoveLeader(ctx, transfereeID)
	}
}

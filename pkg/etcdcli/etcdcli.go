package etcdcli

import (
	"context"
	"fmt"
	"net"
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
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/api/v3rpc/rpctypes"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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
}

func (e *etcdClientGetter) Get() ([]string, error) {
	if !e.nodeListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}
	if !e.endpointsListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}
	if !e.networkListerSynced() {
		return nil, fmt.Errorf("network lister not synced")
	}

	network, err := e.networkLister.Get("cluster")
	if err != nil {
		return nil, err
	}

	etcdEndpoints := []string{}
	nodes, err := e.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	for _, node := range nodes {
		internalIP, err := dnshelpers.GetEscapedPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return nil, err
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", internalIP))
	}

	hostEtcd, err := e.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd-2")
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
	return etcdEndpoints, nil
}

type etcdClient struct {
	EtcdEndpointsGetter

	// TODO: need to find a way to make controllers specify their own even recorder
	//       instead of generic event recorder, so the events can be back traced to
	//      the controller calling the client methods
	eventRecorder events.Recorder

	clientLock          sync.Mutex
	lastClientConfigKey []string
	cachedClient        *clientv3.Client
}

func NewEtcdClient(kubeInformers v1helpers.KubeInformersForNamespaces, networkInformer configv1informers.NetworkInformer, eventRecorder events.Recorder) EtcdClient {
	return &etcdClient{
		EtcdEndpointsGetter: &etcdClientGetter{
			nodeLister:            kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
			endpointsLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
			networkLister:         networkInformer.Lister(),
			nodeListerSynced:      kubeInformers.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
			endpointsListerSynced: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
			networkListerSynced:   networkInformer.Informer().HasSynced,
		},
		eventRecorder: eventRecorder.WithComponentSuffix("etcd-client"),
	}
}

// getCachedEtcdClient may return a cached client.  When a new client is needed, the previous client is closed.
// The caller should not close the client or future calls may fail.
func (e *etcdClient) getCachedEtcdClient() (*clientv3.Client, error) {
	etcdEndpoints, err := e.Get()
	if err != nil {
		return nil, err
	}

	e.clientLock.Lock()
	defer e.clientLock.Unlock()
	// check if the connection is already closed
	if reflect.DeepEqual(e.lastClientConfigKey, etcdEndpoints) &&
		e.cachedClient.ActiveConnection().GetState() == connectivity.Ready {
		return e.cachedClient, nil
	}

	c, err := getEtcdClient(etcdEndpoints)
	if err != nil {
		return nil, err
	}
	if e.cachedClient != nil {
		if err := e.cachedClient.Close(); err != nil {
			utilruntime.HandleError(err)
		}
	}
	e.cachedClient = c
	e.lastClientConfigKey = etcdEndpoints

	// set endpoints to existing members
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = e.cachedClient.Sync(ctx)
	if err != nil {
		return nil, fmt.Errorf("error syncing endpoints to existing members: %v", err)
	}

	return e.cachedClient, nil
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

func (e *etcdClient) MemberAdd(peerURL string) error {
	e.eventRecorder.Eventf("MemberAdd", "adding new peer %v", peerURL)

	cli, err := e.getCachedEtcdClient()
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
				e.eventRecorder.Warningf("MemberAlreadyAdded", "member with peerURL %s already part of the cluster", peerURL)
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

func (e *etcdClient) MemberUpdatePeerURL(id uint64, peerURLs []string) error {
	if members, err := e.MemberList(); err != nil {
		e.eventRecorder.Eventf("MemberUpdate", "updating member %d with peers %v", id, strings.Join(peerURLs, ","))
	} else {
		memberName := fmt.Sprintf("%d", id)
		for _, member := range members {
			if member.ID == id {
				memberName = member.Name
				break
			}
		}
		e.eventRecorder.Eventf("MemberUpdate", "updating member %q with peers %v", memberName, strings.Join(peerURLs, ","))
	}

	cli, err := e.getCachedEtcdClient()
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

func (e *etcdClient) MemberRemove(member string) error {
	e.eventRecorder.Eventf("MemberRemove", "removing member %q", member)

	cli, err := e.getCachedEtcdClient()
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

	e.eventRecorder.Warningf("MemberAlreadyRemoved", "member %q already removed", member)
	return nil
}

func (e *etcdClient) MemberList() ([]*etcdserverpb.Member, error) {
	cli, err := e.getCachedEtcdClient()
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

func (e *etcdClient) GetMember(name string) (*etcdserverpb.Member, error) {
	members, err := e.MemberList()
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

func (e *etcdClient) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	cli, err := e.getCachedEtcdClient()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	unhealthyMembers := []*etcdserverpb.Member{}
	unstartedMemberNames := []string{}
	unhealthyMemberNames := []string{}
	for _, member := range membersResp.Members {
		if len(member.ClientURLs) == 0 {
			unhealthyMembers = append(unhealthyMembers, member)
			unstartedMemberNames = append(unstartedMemberNames, member.Name)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// TODO: explore caching clients per member when adding or removing
		//       to improve performance
		// get the client for the member
		memberClient, err := getEtcdClient([]string{member.ClientURLs[0]})
		if err != nil {
			unhealthyMembers = append(unhealthyMembers, member)
			unhealthyMemberNames = append(unhealthyMemberNames, member.Name)
			continue
		}

		// get a key to check for linearized operation
		_, err = memberClient.Get(ctx, "health")
		switch {
		case err != nil && err != rpctypes.ErrPermissionDenied:
			// invalid certs panic so that operator pod gets right certs on restart
			panic("Invalid Certs")
		case err != nil:
			unhealthyMembers = append(unhealthyMembers, member)
			unhealthyMemberNames = append(unhealthyMemberNames, member.Name)
		}
	}

	if len(unstartedMemberNames) > 0 {
		e.eventRecorder.Warningf("UnstartedEtcdMember", "unstarted members: %v", strings.Join(unstartedMemberNames, ","))
	}

	if len(unhealthyMemberNames) > 0 {
		e.eventRecorder.Warningf("UnhealthyEtcdMember", "unhealthy members: %v", strings.Join(unhealthyMemberNames, ","))
	}

	return unhealthyMembers, nil
}

func (e *etcdClient) MemberStatus(member *etcdserverpb.Member) string {
	cli, err := e.getCachedEtcdClient()
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

func (e *etcdClient) SetEventRecorder(eventRecorder events.Recorder) {
	e.eventRecorder = eventRecorder
}

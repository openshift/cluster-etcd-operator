package etcdcli

import (
	"context"
	"fmt"
	"net"
	"time"

	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func NewEtcdClient(kubeInformers v1helpers.KubeInformersForNamespaces, networkInformer configv1informers.NetworkInformer) EtcdClient {
	return &etcdClientGetter{
		nodeLister:            kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		endpointsLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		networkLister:         networkInformer.Lister(),
		nodeListerSynced:      kubeInformers.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		endpointsListerSynced: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
		networkListerSynced:   networkInformer.Informer().HasSynced,
	}
}

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
	if bootstrapIP := hostEtcd.Annotations[BootstrapIPAnnotationKey]; bootstrapIP != "" {
		// escape if IPv6
		if net.ParseIP(bootstrapIP).To4() == nil {
			bootstrapIP = "[" + bootstrapIP + "]"
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", bootstrapIP))
	}
	c, err := getEtcdClient(etcdEndpoints)
	if err != nil {
		return nil, err
	}

	return c, nil
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
	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return err
	}

	for _, member := range membersResp.Members {
		for _, currPeerURL := range member.PeerURLs {
			if currPeerURL == peerURL {
				klog.V(2).Infof("member with peerURL %s already part of the cluster", peerURL)
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
	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err = cli.MemberUpdate(ctx, id, peerURLs)
	if err != nil {
		return err
	}
	return err
}

func (g *etcdClientGetter) MemberRemove(member string) error {
	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}
	defer cli.Close()

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

	return nil
}

func (g *etcdClientGetter) MemberList() ([]*etcdserverpb.Member, error) {
	cli, err := g.getEtcdClient()
	if err != nil {
		return nil, err
	}
	defer cli.Close()

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

func (g *etcdClientGetter) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	cli, err := g.getEtcdClient()
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	membersResp, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}

	unhealthyMembers := []*etcdserverpb.Member{}
	for _, member := range membersResp.Members {
		if len(member.ClientURLs) == 0 {
			unhealthyMembers = append(unhealthyMembers, member)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		_, err := cli.Status(ctx, member.ClientURLs[0])
		if err != nil {
			unhealthyMembers = append(unhealthyMembers, member)
		}
	}

	return unhealthyMembers, nil
}

func (g *etcdClientGetter) MemberStatus(member *etcdserverpb.Member) string {
	cli, err := g.getEtcdClient()
	if err != nil {
		klog.Errorf("error getting etcd client: %#v", err)
		return EtcdMemberStatusUnknown
	}
	defer cli.Close()

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

package etcdcli

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

type etcdClientGetter struct {
	nodeLister      corev1listers.NodeLister
	endpointsLister corev1listers.EndpointsLister

	nodeListerSynced      cache.InformerSynced
	endpointsListerSynced cache.InformerSynced
}

func NewEtcdClient(kubeInformers v1helpers.KubeInformersForNamespaces) EtcdClient {
	return &etcdClientGetter{
		nodeLister:            kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		endpointsLister:       kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Lister(),
		nodeListerSynced:      kubeInformers.InformersFor("").Core().V1().Nodes().Informer().HasSynced,
		endpointsListerSynced: kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Endpoints().Informer().HasSynced,
	}
}

func (g *etcdClientGetter) getEtcdClient() (*clientv3.Client, error) {
	if !g.nodeListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}
	if !g.endpointsListerSynced() {
		return nil, fmt.Errorf("node lister not synced")
	}

	etcdEndpoints := []string{}
	nodes, err := g.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	for _, node := range nodes {
		internalIP, err := getInternalIPAddressForNodeName(node)
		if err != nil {
			return nil, err
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", internalIP))
	}

	hostEtcd, err := g.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
	if err != nil {
		return nil, err
	}
	for _, addr := range hostEtcd.Subsets[0].Addresses {
		if addr.Hostname == "etcd-bootstrap" {
			// etcd-bootstrap has a valid IP in host-etcd
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", addr.IP))
			break
		}
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

func getInternalIPAddressForNodeName(node *corev1.Node) (string, error) {
	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalIP {
			return currAddress.Address, nil
		}
	}
	return "", fmt.Errorf("node/%s missing %s", node.Name, corev1.NodeInternalIP)
}

func (g *etcdClientGetter) MemberAdd(peerURL string) error {
	cli, err := g.getEtcdClient()
	if err != nil {
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	membersResp, err := cli.MemberList(ctx)
	cancel()
	if err != nil {
		return err
	}

	for _, member := range membersResp.Members {
		if len(member.PeerURLs) > 0 && member.PeerURLs[0] == peerURL {
			klog.V(2).Infof("member with peerURL %s already part of the cluster", peerURL)
			return nil
		}
	}

	_, err = cli.MemberAdd(ctx, []string{peerURL})
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
	membersResp, err := cli.MemberList(ctx)
	cancel()
	if err != nil {
		return nil
	}

	for _, m := range membersResp.Members {
		if m.Name == member {
			ctx, cancel := context.WithCancel(context.Background())
			_, err = cli.MemberRemove(ctx, m.ID)
			cancel()
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
	membersResp, err := cli.MemberList(ctx)
	cancel()
	if err != nil {
		return nil, err
	}

	return membersResp.Members, nil
}

func (g *etcdClientGetter) UnhealthyMembers() ([]*etcdserverpb.Member, error) {
	cli, err := g.getEtcdClient()
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	membersResp, err := cli.MemberList(ctx)
	cancel()
	if err != nil {
		return nil, err
	}

	unhealthyMembers := []*etcdserverpb.Member{}
	for _, member := range membersResp.Members {
		if len(member.ClientURLs) == 0 {
			unhealthyMembers = append(unhealthyMembers, member)
		}
		ctx, cancel := context.WithCancel(context.Background())
		_, err := cli.Status(ctx, member.ClientURLs[0])
		cancel()
		if err != nil {
			unhealthyMembers = append(unhealthyMembers, member)
		}
	}

	return unhealthyMembers, nil
}

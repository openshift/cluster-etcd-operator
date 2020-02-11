package etcdcli

import (
	"fmt"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

func GetEtcdClient(nodeLister corev1listers.NodeLister, endpointsLister corev1listers.EndpointsLister) (*clientv3.Client, error) {

	hostEtcd, err := endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
	if err != nil {
		return nil, err
	}

	if len(hostEtcd.Subsets) == 0 {
		return nil, fmt.Errorf("invalid ip address for host-etcd endpoint")
	}

	etcdEndpoints := []string{}
	for _, addr := range hostEtcd.Subsets[0].Addresses {
		if addr.Hostname == "etcd-bootstrap" {
			// etcd-bootstrap has a valid IP in host-etcd
			etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", addr.IP))
			break
		}
	}

	nodes, err := nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	for _, node := range nodes {
		internalIP, err := getInternalIPAddressForNodeName(node)
		if err != nil {
			return nil, err
		}
		etcdEndpoints = append(etcdEndpoints, fmt.Sprintf("https://%s:2379", internalIP))
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

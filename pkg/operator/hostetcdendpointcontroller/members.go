package hostetcdendpointcontroller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/pkg/transport"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog"
)

const (
	etcdCertFile      = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile       = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
)

type HealthyEtcdMembersGetter interface {
	GetHealthyEtcdMembers() ([]string, error)
}

type healthyEtcdMemberGetter struct {
	operatorConfigClient v1helpers.OperatorClient
}

func NewHealthyEtcdMemberGetter(operatorConfigClient v1helpers.OperatorClient) HealthyEtcdMembersGetter {
	return &healthyEtcdMemberGetter{operatorConfigClient}
}

func (h *healthyEtcdMemberGetter) GetHealthyEtcdMembers() ([]string, error) {
	member, err := h.EtcdList("members")
	if err != nil {
		return nil, err
	}
	hostnames := make([]string, 0)
	for _, m := range member {
		hostname, err := getEtcdName(m.PeerURLS[0])
		if err != nil {
			return nil, err
		}
		hostnames = append(hostnames, hostname)
	}
	return hostnames, nil
}

// getEtcdName returns the name of the peer from a valid peerURL
func getEtcdName(peerURL string) (string, error) {
	if peerURL == "" {
		return "", fmt.Errorf("getEtcdName: peerURL is empty")
	}
	if strings.Contains(peerURL, "etcd-") {
		return strings.TrimPrefix(strings.Split(peerURL, ".")[0], "https://"), nil
	}
	u, err := url.Parse(peerURL)
	if err != nil {
		return "", err
	}
	host, port, _ := net.SplitHostPort(u.Host)
	//TODO peer port should be a global constant
	if IsIP(host) && port == "2380" {
		return "etcd-bootstrap", nil
	}
	return "", fmt.Errorf("getEtcdName: peerURL %q is not properly formatted", peerURL)
}

func (h *healthyEtcdMemberGetter) EtcdList(bucket string) ([]ceoapi.Member, error) {
	configPath := []string{"cluster", bucket}
	operatorSpec, _, _, err := h.operatorConfigClient.GetOperatorState()
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(operatorSpec.ObservedConfig.Raw)).Decode(&config); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}
	data, exists, err := unstructured.NestedSlice(config, configPath...)
	if err != nil {
		return nil, err
	}
	members := []ceoapi.Member{}
	if !exists {
		return members, nil
	}

	// populate current etcd members as observed.
	for _, member := range data {
		memberMap, _ := member.(map[string]interface{})
		name, exists, err := unstructured.NestedString(memberMap, "name")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member name does not exist")
		}
		peerURLs, exists, err := unstructured.NestedString(memberMap, "peerURLs")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member peerURLs do not exist")
		}
		// why have different terms i.e. status and condition? can we choose one and mirror?
		status, exists, err := unstructured.NestedString(memberMap, "status")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member status does not exist")
		}

		condition := ceoapi.GetMemberCondition(status)
		if condition == ceoapi.MemberReady {
			m := ceoapi.Member{
				Name:     name,
				PeerURLS: []string{peerURLs},
				Conditions: []ceoapi.MemberCondition{
					{
						Type: condition,
					},
				},
			}
			members = append(members, m)
		}
	}
	return members, nil
}

func (h *healthyEtcdMemberGetter) getEtcdClient() (*clientv3.Client, error) {
	endpoints, err := h.Endpoints()
	if err != nil {
		return nil, err
	}
	tlsInfo := transport.TLSInfo{
		CertFile:      etcdCertFile,
		KeyFile:       etcdKeyFile,
		TrustedCAFile: etcdTrustedCAFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()

	cfg := &clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 2 * time.Second,
		TLS:         tlsConfig,
	}

	cli, err := clientv3.New(*cfg)
	if err != nil {
		return nil, err
	}
	return cli, err
}

func (h *healthyEtcdMemberGetter) Endpoints() ([]string, error) {
	storageConfigURLsPath := []string{"storageConfig", "urls"}
	operatorSpec, _, _, err := h.operatorConfigClient.GetOperatorState()
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(operatorSpec.ObservedConfig.Raw)).Decode(&config); err != nil {
		klog.V(4).Infof("decode of existing config failed with error: %v", err)
	}
	endpoints, exists, err := unstructured.NestedStringSlice(config, storageConfigURLsPath...)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("etcd storageConfig urls not observed")
	}

	return endpoints, nil
}

//TODO add to util
func IsIP(addr string) bool {
	if ip := net.ParseIP(addr); ip != nil {
		return true
	}
	return false
}

package hostetcdendpointcontroller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/pkg/transport"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
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
		hostname := getEtcdName(m.PeerURLS)
		hostnames = append(hostnames, hostname)
	}
	return hostnames, nil
}

func getEtcdName(peerURLs []string) string {
	for _, peerURL := range peerURLs {
		if strings.Contains(peerURL, "etcd-") {
			return strings.TrimPrefix(strings.Split(peerURLs[0], ".")[0], "https://")
		}
	}
	return ""
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
	if !exists {
		return nil, fmt.Errorf("etcd cluster members not observed")
	}

	// populate current etcd members as observed.
	var members []ceoapi.Member
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

package aio

import (
	"fmt"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
)

func getAIOEtcdEnvVars(nodeName, nodeInternalIP, imagePullSpec string) (map[string]string, error) {
	ret := map[string]string{
		"ETCDCTL_API":             "3",
		"ETCDCTL_CACERT":          "/etc/kubernetes/static-pod-certs/configmaps/etcd-serving-ca/ca-bundle.crt",
		"ETCDCTL_CERT":            "/etc/kubernetes/static-pod-certs/secrets/etcd-all-peer/etcd-peer-NODE_NAME.crt",
		"ETCDCTL_KEY":             "/etc/kubernetes/static-pod-certs/secrets/etcd-all-peer/etcd-peer-NODE_NAME.key",
		"ETCDCTL_ENDPOINTS":       fmt.Sprintf("https://%s:2379", nodeInternalIP),
		"ALL_ETCD_ENDPOINTS":      fmt.Sprintf("https://%s:2379", nodeInternalIP),
		"ETCD_IMAGE":              imagePullSpec,
		"ETCD_HEARTBEAT_INTERVAL": "100",  // etcd default
		"ETCD_ELECTION_TIMEOUT":   "1000", // etcd default

		fmt.Sprintf("NODE_%s_ETCD_NAME", nodeName):     nodeName,
		fmt.Sprintf("NODE_%s_IP", nodeName):            nodeInternalIP,
		fmt.Sprintf("NODE_%s_ETCD_URL_HOST", nodeName): nodeInternalIP,
	}
	for k, v := range etcdenvvar.FixedEtcdEnvVars {
		ret[k] = v
	}
	return ret, nil
}

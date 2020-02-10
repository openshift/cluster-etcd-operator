package clustermembercontroller

import (
	"time"
)

const (
	workQueueKey                   = "key"
	EtcdScalingAnnotationKey       = "etcd.operator.openshift.io/scale"
	etcdCertFile                   = "/var/run/secrets/etcd-client/tls.crt"
	etcdKeyFile                    = "/var/run/secrets/etcd-client/tls.key"
	etcdTrustedCAFile              = "/var/run/configmaps/etcd-ca/ca-bundle.crt"
	EtcdEndpointNamespace          = "openshift-etcd"
	EtcdHostEndpointName           = "host-etcd"
	EtcdEndpointName               = "etcd"
	ConditionBootstrapSafeToRemove = "BootstrapSafeToRemove"
	dialTimeout                    = 20 * time.Second
)

module github.com/openshift/cluster-etcd-operator

go 1.13

require (
	github.com/davecgh/go-spew v1.1.1
	github.com/ghodss/yaml v1.0.0
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/openshift/api v0.0.0-20200723134351-89de68875e7c
	github.com/openshift/build-machinery-go v0.0.0-20200713135615-1f43d26dccc7
	github.com/openshift/client-go v0.0.0-20200722173614-5a1b0aaeff15
	github.com/openshift/library-go v0.0.0-20200526124911-cd27f9384ffc
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.7.1
	github.com/prometheus/common v0.10.0
	github.com/spf13/cobra v1.0.0
	github.com/spf13/pflag v1.0.5
	github.com/vishvananda/netlink v1.0.0
	go.etcd.io/etcd v0.5.0-alpha.5.0.20200716221620-18dfb9cca345
	golang.org/x/sys v0.0.0-20200622214017-ed371f2e16b4
	google.golang.org/grpc v1.27.0
	k8s.io/api v0.19.0-rc.2
	k8s.io/apiextensions-apiserver v0.19.0-rc.2
	k8s.io/apimachinery v0.19.0-rc.2
	k8s.io/client-go v0.19.0-rc.2
	k8s.io/component-base v0.19.0-rc.2
	k8s.io/klog v1.0.0
	k8s.io/utils v0.0.0-20200720150651-0bdb4ca86cbc
)

replace github.com/openshift/library-go => github.com/hexfusion/library-go v0.0.0-20200730105656-75f39e07357d

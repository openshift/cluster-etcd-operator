module github.com/openshift/cluster-etcd-operator

go 1.13

require (
	github.com/cloudflare/cfssl v1.4.1
	github.com/davecgh/go-spew v1.1.1
	github.com/ghodss/yaml v1.0.0
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/gorilla/mux v0.0.0-20191024121256-f395758b854c
	github.com/openshift/api v0.0.0-20200821140346-b94c46af3f2b
	github.com/openshift/build-machinery-go v0.0.0-20200512074546-3744767c4131
	github.com/openshift/client-go v0.0.0-20200521150516-05eb9880269c
	github.com/openshift/library-go v0.0.0-20200904081757-57d8acacca26
	github.com/prometheus/client_golang v1.1.0
	github.com/prometheus/common v0.6.0
	github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag v1.0.5
	github.com/vincent-petithory/dataurl v0.0.0-20191104211930-d1553a71de50
	github.com/vishvananda/netlink v1.0.0
	go.etcd.io/etcd v0.0.0-20200401174654-e694b7bb0875
	golang.org/x/sys v0.0.0-20200323222414-85ca7c5b95cd
	google.golang.org/grpc v1.26.0
	k8s.io/api v0.18.3
	k8s.io/apimachinery v0.18.3
	k8s.io/client-go v0.18.3
	k8s.io/component-base v0.18.3
	k8s.io/klog v1.0.0
	k8s.io/utils v0.0.0-20200324210504-a9aa75ae1b89
)

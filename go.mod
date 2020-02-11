module github.com/openshift/cluster-etcd-operator

go 1.13

require (
	github.com/cloudflare/cfssl v1.4.1
	github.com/ghodss/yaml v1.0.0
	github.com/gorilla/mux v0.0.0-20191024121256-f395758b854c
	github.com/jteeuwen/go-bindata v3.0.8-0.20151023091102-a0ff2567cfb7+incompatible
	github.com/openshift/api v0.0.0-20200131223221-f2a771e1a90c
	github.com/openshift/build-machinery-go v0.0.0-20200210090402-3b072832771e
	github.com/openshift/client-go v0.0.0-20200116152001-92a2713fa240
	github.com/openshift/library-go v0.0.0-20200207150939-615337e1c3aa
	github.com/prometheus/client_golang v1.1.0
	github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag v1.0.5
	github.com/vincent-petithory/dataurl v0.0.0-20191104211930-d1553a71de50
	go.etcd.io/etcd v0.0.0-20191023171146-3cf2f69b5738
	google.golang.org/grpc v1.23.1
	k8s.io/api v0.17.1
	k8s.io/apimachinery v0.17.1
	k8s.io/client-go v0.17.1
	k8s.io/component-base v0.17.1
	k8s.io/klog v1.0.0
)

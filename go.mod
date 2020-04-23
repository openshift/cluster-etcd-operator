module github.com/openshift/cluster-etcd-operator

go 1.13

require (
	github.com/cloudflare/cfssl v1.4.1
	github.com/davecgh/go-spew v1.1.1
	github.com/ghodss/yaml v1.0.0
	github.com/gorilla/mux v0.0.0-20191024121256-f395758b854c
	github.com/jteeuwen/go-bindata v3.0.8-0.20151023091102-a0ff2567cfb7+incompatible
	github.com/openshift/api v0.0.0-20200326160804-ecb9283fe820
	github.com/openshift/build-machinery-go v0.0.0-20200211121458-5e3d6e570160
	github.com/openshift/client-go v0.0.0-20200326155132-2a6cd50aedd0
	github.com/openshift/library-go v0.0.0-20200422120251-a5cb46356745
	github.com/prometheus/client_golang v1.1.0
	github.com/prometheus/common v0.6.0
	github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag v1.0.5
	github.com/vincent-petithory/dataurl v0.0.0-20191104211930-d1553a71de50
	go.etcd.io/etcd v0.0.0-20200401174654-e694b7bb0875
	google.golang.org/grpc v1.26.0
	k8s.io/api v0.18.0
	k8s.io/apimachinery v0.18.0
	k8s.io/client-go v0.18.0
	k8s.io/component-base v0.18.0
	k8s.io/klog v1.0.0
)

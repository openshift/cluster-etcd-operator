FROM registry.svc.ci.openshift.org/openshift/release:golang-1.12 AS builder
WORKDIR /go/src/github.com/openshift/cluster-etcd-operator
COPY . .

ENV GO111MODULE=on
RUN go build -mod vendor ./cmd/cluster-etcd-operator

FROM registry.svc.ci.openshift.org/openshift/origin-v4.0:base
RUN mkdir -p /usr/share/bootkube/manifests
COPY --from=builder /go/src/github.com/openshift/cluster-etcd-operator/bindata/bootkube/* /usr/share/bootkube/manifests/
COPY --from=builder /go/src/github.com/openshift/cluster-etcd-operator/bindata/bootkube/config/* /usr/share/bootkube/manifests/config/
COPY --from=builder /go/src/github.com/openshift/cluster-etcd-operator/bindata/bootkube/manifests/* /usr/share/bootkube/manifests/manifests/
COPY --from=builder /go/src/github.com/openshift/cluster-etcd-operator/bindata/bootkube/bootstrap-manifests/* /usr/share/bootkube/manifests/bootstrap-manifests/
COPY --from=builder /go/src/github.com/openshift/cluster-etcd-operator/cluster-etcd-operator /usr/bin/
COPY manifests/* /manifests/
LABEL io.openshift.release.operator=true

# cluster-etcd-operator

The kube-controller-manager operator installs and maintains the kube-controller-manager on a cluster

## Developing and debugging the bootkube bootstrap phase

The operator image version used by the [https://github.com/openshift/installer/blob/master/pkg/asset/ignition/bootstrap/content/bootkube.go#L86](installer) bootstrap phase can be overridden by creating a custom origin-release image pointing to the developer's operator `:latest` image:

```
$ IMAGE_ORG=sttts make images
$ docker push sttts/origin-cluster-etcd-operator

$ cd ../cluster-kube-apiserver-operator
$ IMAGES=cluster-etcd-operator IMAGE_ORG=sttts make origin-release
$ docker push sttts/origin-release:latest

$ cd ../installer
$ OPENSHIFT_INSTALL_RELEASE_IMAGE_OVERRIDE=docker.io/sttts/origin-release:latest bin/openshift-install cluster ...
```

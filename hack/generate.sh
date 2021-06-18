#!/bin/sh
set -eux

# Generate jsonnet mixin prometheusrule manifest.

cd jsonnet && jb update && jsonnet -J vendor main.jsonnet  | gojsontoyaml > ../manifests/0000_90_etcd-operator_03_prometheusrule.yaml

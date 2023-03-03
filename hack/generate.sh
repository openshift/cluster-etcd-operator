#!/bin/sh
set -eu

if ! command -v jb &> /dev/null; then
  echo "jb could not be found. See https://github.com/jsonnet-bundler/jsonnet-bundler"
  exit 1
fi

# Generate jsonnet mixin prometheusrule and dashboards manifest.

cd jsonnet && jb update
jsonnet -J vendor main.jsonnet  | gojsontoyaml > ../manifests/0000_90_etcd-operator_03_prometheusrule.yaml
jsonnet -J vendor dashboard.jsonnet  | gojsontoyaml > ../manifests/0000_90_cluster-etcd-operator_01-dashboards.yaml
jsonnet -J vendor removed_dashboard.jsonnet  | gojsontoyaml > ../manifests/0000_90_cluster-etcd-operator_02-removed-dashboard.yaml
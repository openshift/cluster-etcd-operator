#!/bin/sh
set -eu

if ! command -v jsonnet &> /dev/null; then
  echo "jsonnet could not be found. See https://github.com/google/jsonnet"
  exit 1
fi

if ! command -v jb &> /dev/null; then
  echo "jb could not be found. See https://github.com/jsonnet-bundler/jsonnet-bundler"
  exit 1
fi

if ! command -v gojsontoyaml &> /dev/null; then
  echo "gojsontoyaml could not be found. See https://github.com/brancz/gojsontoyaml"
  exit 1
fi

# Generate jsonnet mixin prometheusrule and dashboards manifest.

cd jsonnet && jb update
jsonnet -J vendor main.jsonnet  | gojsontoyaml > ../manifests/0000_90_etcd-operator_03_prometheusrule.yaml
jsonnet -J vendor dashboard.jsonnet  | gojsontoyaml > ../manifests/0000_90_cluster-etcd-operator_01-dashboards.yaml

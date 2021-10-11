#!/bin/sh
set -eu

if ! command -v jb &> /dev/null; then
  echo "jb could not be found. See https://github.com/jsonnet-bundler/jsonnet-bundler"
  exit 1
fi

# Generate jsonnet mixin prometheusrule manifest.

cd jsonnet && jb update && jsonnet -J vendor main.jsonnet  | gojsontoyaml > ../manifests/0000_90_etcd-operator_03_prometheusrule.yaml

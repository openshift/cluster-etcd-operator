#!/usr/bin/env bash
# Extract Prometheus rule groups from the CVO PrometheusRule manifest and run
# promtool unit tests for backup alert vector-matching behavior.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFEST="${REPO_ROOT}/manifests/0000_90_etcd-operator_03_prometheusrule.yaml"
TEST_DIR="${REPO_ROOT}/jsonnet"
RULES_FOR_TEST="${TEST_DIR}/backup_alerts_rules_for_test.yaml"

if ! command -v promtool >/dev/null 2>&1; then
  echo "promtool not found; install with:" >&2
  echo "  go install github.com/prometheus/prometheus/cmd/promtool@latest" >&2
  exit 1
fi

go run "${REPO_ROOT}/hack/extract_prometheus_rules.go" "${MANIFEST}" "${RULES_FOR_TEST}"

cd "${TEST_DIR}"
promtool test rules backup_alerts_test.yaml
echo "backup alert rule tests passed"

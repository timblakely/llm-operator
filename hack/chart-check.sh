#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
chart_dir="${root_dir}/charts/llm-operator"
helm="${HELM:-helm}"
kube_version="${HELM_KUBE_VERSION:-1.35.0}"
test_values="${chart_dir}/ci/test-values.yaml"
rendered="$(mktemp)"
enabled="$(mktemp)"
trap 'rm -f "${rendered}" "${enabled}"' EXIT

status=0
for source in "${root_dir}"/config/crd/llm.cogito.dev_*.yaml; do
  chart_crd="${chart_dir}/crds/$(basename "${source}")"
  if ! diff -u "${source}" "${chart_crd}"; then
    status=1
  fi
done
if ((status != 0)); then
  echo "chart CRDs are stale; run 'make chart-crds-sync'" >&2
  exit "${status}"
fi

"${helm}" lint "${chart_dir}" --kube-version "${kube_version}" -f "${test_values}"

if "${helm}" template llm-operator "${chart_dir}" --namespace llm --kube-version "${kube_version}" >/dev/null 2>&1; then
  echo "chart check failed: rendering without image.digest unexpectedly succeeded" >&2
  exit 1
fi

"${helm}" template llm-operator "${chart_dir}" \
  --namespace llm \
  --kube-version "${kube_version}" \
  --include-crds \
  -f "${test_values}" >"${rendered}"

assert_count() {
  local expected="$1"
  local pattern="$2"
  local description="$3"
  local actual
  actual="$(grep -Ec -- "${pattern}" "${rendered}" || true)"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "chart check failed: expected ${expected} ${description}, found ${actual}" >&2
    exit 1
  fi
}

assert_count 4 '^kind: CustomResourceDefinition$' 'CRDs'
assert_count 1 '^kind: Deployment$' 'manager Deployment'
assert_count 1 '^kind: ServiceAccount$' 'ServiceAccount'
assert_count 1 '^kind: ClusterRole$' 'ClusterRole'
assert_count 1 '^kind: ClusterRoleBinding$' 'ClusterRoleBinding'
assert_count 1 '^kind: Service$' 'metrics Service'
assert_count 1 '^kind: PodDisruptionBudget$' 'PodDisruptionBudget'
assert_count 1 '--enable-transitions=false' 'disabled transition arguments'
assert_count 0 '--enable-transitions=true' 'enabled transition arguments'
assert_count 1 'image: "ghcr.io/timblakely/llm-operator@sha256:[0-9a-f]{64}"' 'digest-pinned manager images'
assert_count 0 '^kind: (LLMModel|LLMModelOverlay|LLMActiveModel|LLMBackend)$' 'sample or live custom resources'
assert_count 0 'name: CACHE_MANAGER_URL' 'default cache-manager environment variables'

"${helm}" template llm-operator "${chart_dir}" \
  --namespace llm \
  --kube-version "${kube_version}" \
  -f "${test_values}" \
  --set transitions.enabled=true \
  --set-string cacheManager.url=http://cache-manager.llm.svc:8090 >"${enabled}"

if [[ "$(grep -c -- '--enable-transitions=true' "${enabled}" || true)" != "1" ]]; then
  echo "chart check failed: transitions.enabled=true was not rendered" >&2
  exit 1
fi
if [[ "$(grep -c -- 'name: CACHE_MANAGER_URL' "${enabled}" || true)" != "1" ]]; then
  echo "chart check failed: cacheManager.url was not rendered" >&2
  exit 1
fi

echo "chart check passed: CRDs are current and the chart is digest-pinned in observation mode"

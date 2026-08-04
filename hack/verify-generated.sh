#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
controller_gen="${CONTROLLER_GEN:-${root_dir}/bin/controller-gen}"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

cd "${root_dir}"

for api_version in v1alpha1 v1beta1; do
  "${controller_gen}" \
    object:headerFile="hack/boilerplate.go.txt",year=2025 \
    paths="./api/cogito.dev/${api_version}" \
    output:dir="${tmp_dir}/object/${api_version}"
done
"${controller_gen}" \
  crd \
  paths="./api/..." \
  output:crd:artifacts:config="${tmp_dir}/crd"
"${controller_gen}" \
  rbac:roleName=manager-role \
  paths="./internal/controller/..." \
  output:rbac:artifacts:config="${tmp_dir}/rbac"

status=0
compare_file() {
  local checked_in="$1"
  local generated="$2"

  if ! diff -u "${checked_in}" "${generated}"; then
    status=1
  fi
}

for api_version in v1alpha1 v1beta1; do
  compare_file \
    "api/cogito.dev/${api_version}/zz_generated.deepcopy.go" \
    "${tmp_dir}/object/${api_version}/zz_generated.deepcopy.go"
done

for checked_in in config/crd/llm.cogito.dev_*.yaml; do
  compare_file "${checked_in}" "${tmp_dir}/crd/$(basename "${checked_in}")"
done

compare_file "config/rbac/role.yaml" "${tmp_dir}/rbac/role.yaml"

if ((status != 0)); then
  echo "generated files are stale; run 'make manifests generate'" >&2
  exit "${status}"
fi

echo "generated CRDs, RBAC, and deepcopy code are current"

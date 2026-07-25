#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kustomize="${KUSTOMIZE:-${root_dir}/bin/kustomize}"
rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT

"${kustomize}" build "${root_dir}/config/default" >"${rendered}"

disabled_count="$(grep -c -- '--enable-transitions=false' "${rendered}" || true)"
if [[ "${disabled_count}" != "1" ]]; then
  echo "observation preflight failed: expected exactly one --enable-transitions=false argument, found ${disabled_count}" >&2
  exit 1
fi

if grep -q -- '--enable-transitions=true' "${rendered}"; then
  echo "observation preflight failed: rendered manifest enables transitions" >&2
  exit 1
fi

echo "observation preflight passed: transitions are explicitly disabled"

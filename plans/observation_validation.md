# Observation-Mode Cluster Validation

## Status

**Blocked on 2026-07-27 before deployment. No cluster resources were changed.**

The configured Kubernetes context is `admin@nuglab`, with API endpoint
`https://10.0.1.69:6443`. Both an authenticated `kubectl get` and an unauthenticated
`GET /livez` timed out. The host has a route through `192.168.10.1`, but the API
endpoint did not accept a TCP connection.

The original preflight also found the then-configured mutable manager image tag
unavailable:

```text
ghcr.io/timblakely/llm-operator:latest: manifest unknown
```

The manifest now uses a newer development tag, but it has not yet been verified
from the target cluster and is not pinned to an immutable digest. Because the
API inventory could not be read and a reviewed image/chart artifact was not
available, CRDs, RBAC, manager resources, and sample CRs were not applied.
Backend Deployments, cache-manager, and the proxy were not contacted or mutated.

There is also a validation-scope gap in the current controller behavior:
observation mode records backend health and activation annotations, but runtime
metadata collection occurs only after an operator-owned transition. Likewise,
an `LLMActiveModel` with transitions disabled reports `TransitionsDisabled`; it
does not infer and write the proxy's current active model into its status. Until
passive metadata/status observation is designed, comparison must use
`LLMBackend.status`, Deployment annotations, and the proxy state rather than
`LLMModel.status.runtimeMetadata` or a synthesized active-model status.

## Local safety evidence

- The rendered manager argument is `--enable-transitions=false`.
- `make observation-preflight` fails if the rendered manifest omits that exact
  argument, contains it more than once, or enables transitions.
- The rendered Deployment, RBAC, CRDs, metrics Service, and PDB are internally
  consistent through `make check`.
- `make chart-check` proves the Helm chart CRDs match the generated manifests,
  the manager image is digest-pinned, and transitions render disabled by
  default. The chart is version `0.1.0` at the intended OCI artifact URL
  `oci://ghcr.io/timblakely/charts/llm-operator`.
- Transition mutation remains opt-in in manager code and manifests.

## Preconditions to resume

1. Restore network access to `10.0.1.69:6443` and confirm read access:

   ```bash
   kubectl config current-context
   kubectl get --raw=/livez
   kubectl get namespace llm
   ```

2. Publish the operator image with a reviewed immutable digest, then package
   and publish the already validated chart with `make chart-push`. Run
   `make chart-check`, `make check`, and `make observation-preflight` first.
3. Add an `OCIRepository` and `HelmRelease` in the Cogito GitOps repository.
   The HelmRelease must reference the immutable chart artifact, use
   `CreateReplace` for CRD install/upgrade, and set
   `transitions.enabled=false`.
4. Inventory the real resource names before applying workload CRs:

   ```bash
   kubectl -n llm get deployment,service,pod -o wide
   kubectl -n llm get configmap
   kubectl get crd | grep llm.cogito.dev || true
   ```

5. Review `deploymentRef`, `containerName`, `serviceRef`, port, model source,
   and revision in the sample CRs against that inventory. Do not apply a sample
   merely because its historical name resembles a live workload.

Publishing artifacts, changing cluster connectivity, and committing Cogito
GitOps resources require external action; none is performed by this repository
validation.

## Observation procedure

Capture the transition-owned fields before reconciling the operator chart:

```bash
kubectl -n llm get deployment -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.spec.template.metadata.annotations.llm\.cogito\.dev/active-model}{"\t"}{range .spec.template.spec.containers[*]}{.name}{"="}{.args}{";"}{end}{"\n"}{end}' \
  > /tmp/llm-deployment-fields.before.txt
kubectl -n llm get pod -o wide > /tmp/llm-pods.before.txt
make observation-preflight
flux reconcile source oci llm-operator -n flux-system
flux reconcile helmrelease llm-operator -n llm --with-source
kubectl -n llm rollout status deployment/llm-operator
```

Immediately verify the live Deployment retained the safety flag:

```bash
kubectl -n llm get deployment llm-operator \
  -o jsonpath='{.spec.template.spec.containers[0].args}'
```

After reviewing references, commit only the backend and model CRs that describe
existing workloads to Cogito GitOps and reconcile their owning Kustomization.
Do not use these repository samples as direct imperative applies. Apply
`LLMActiveModel` only after confirming transitions are disabled in the live
manager:

```bash
# Replace <llm-models> with the reviewed Cogito Flux Kustomization name.
flux reconcile kustomization <llm-models> -n flux-system --with-source
```

Validate observed conditions and metadata:

```bash
kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay,llmactivemodel -o wide
kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay,llmactivemodel -o yaml
kubectl -n llm logs deployment/llm-operator --since=1h
```

Expected observation-mode behavior:

- backends report Deployment existence, replica counts, annotations, and health;
- models report configuration and backend resolution;
- overlays report base-model resolution;
- an active-model request reports `TransitionsDisabled` and does not call the
  cache-manager or patch any Deployment;
- runtime metadata is only written after an operator-owned transition, so it is
  not expected to appear during passive observation.

Check operator metrics locally:

```bash
kubectl -n llm port-forward service/llm-operator-metrics 18081:8081
curl --fail http://127.0.0.1:18081/metrics
```

Compare backend annotations and status with the proxy's current catalog/state
using the proxy's existing operational endpoint or logs. Record any canonical
model-name, backend type, replica, health, or active-model mismatch before
cutover work.

After the agreed observation window, prove the workloads did not change:

```bash
kubectl -n llm get deployment -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.spec.template.metadata.annotations.llm\.cogito\.dev/active-model}{"\t"}{range .spec.template.spec.containers[*]}{.name}{"="}{.args}{";"}{end}{"\n"}{end}' \
  > /tmp/llm-deployment-fields.after.txt
diff -u /tmp/llm-deployment-fields.before.txt /tmp/llm-deployment-fields.after.txt
kubectl -n llm get pod -o wide
```

Expected differences are limited to unrelated workload reconciliation and the
new operator Deployment. Existing backend container arguments, replicas, and
operator-owned activation annotations must not change.

## Rollback

The safest rollback leaves CRDs and observations intact and suspends or reverts
the HelmRelease in Cogito GitOps:

```bash
flux suspend helmrelease llm-operator -n llm
```

Commit the equivalent suspension or removal in Cogito GitOps so Flux does not
restore the release. Alternatively, leave the manager running with
`--enable-transitions=false`. The proxy remains the serving controller in both
cases. Do not delete CRDs as an incident response step: that removes observation
data and may interact with model finalizers. Do not change backend,
cache-manager, or proxy workloads as part of operator rollback.

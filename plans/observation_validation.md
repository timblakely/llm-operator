# Observation-Mode Cluster Validation

## Status

**Infrastructure deployment complete on 2026-07-28; workload observation is pending.**

Cogito context `main` (API `https://k8s.internal:6443`) now has a healthy,
Flux-managed operator release in namespace `llm`:

- `OCIRepository/llm-operator` is Ready at chart digest
  `sha256:ca9a4c438302625f10bde4fa0c9df24f0eb8dcd021e4047fd1ea1644fd13b4f5`.
- `HelmRelease/llm-operator` is Ready/Released at chart `0.1.0` (release v2).
- `Deployment/llm-operator` is 2/2 ready and available; both Pods are Running
  with zero restarts and use manager image digest
  `sha256:3ff7d49a889437e17defddd23e36b4acd92ef092f4d5171c0a30f1268e806996`.
- The live manager arguments include exactly `--enable-transitions=false` and
  leader election is active. All four CRDs are Established and controller logs
  have no current warnings or errors.

Initial OCI authorization and image-pull failures occurred during first
installation, but Flux recovered and the current release is healthy. No
`LLMBackend`, `LLMModel`, `LLMModelOverlay`, or `LLMActiveModel` instances exist
yet, so no workload or proxy observation has been performed.

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

## Remaining preconditions for workload observation

1. Confirm the already-deployed release remains healthy before each workload
   change:

   ```bash
   kubectl -n llm get ocirepository/llm-operator helmrelease/llm-operator
   kubectl -n llm rollout status deployment/llm-operator
   ```

2. Inventory the real resource names before committing workload CRs:

   ```bash
   kubectl -n llm get deployment,service,pod -o wide
   kubectl -n llm get configmap
   kubectl get crd | grep llm.cogito.dev || true
   ```

3. Review `deploymentRef`, `containerName`, `serviceRef`, port, model source,
   and revision in the sample CRs against that inventory. Do not apply a sample
   merely because its historical name resembles a live workload.

4. Add only the reviewed workload CRs to Cogito's reserved
   `kubernetes/apps/llm/llm-operator/resources/` directory and include them in
   its Kustomization. Do not add repository samples wholesale.

## Observation procedure

The operator chart is already reconciled. Before adding the first workload CR,
capture a fresh baseline for the observation window:

```bash
kubectl -n llm get deployment -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.spec.template.metadata.annotations.llm\.cogito\.dev/active-model}{"\t"}{range .spec.template.spec.containers[*]}{.name}{"="}{.args}{";"}{end}{"\n"}{end}' \
  > /tmp/llm-deployment-fields.before.txt
kubectl -n llm get pod -o wide > /tmp/llm-pods.before.txt
make observation-preflight
flux reconcile source oci llm-operator -n llm
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

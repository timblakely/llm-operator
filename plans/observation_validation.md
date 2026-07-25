# Observation-Mode Cluster Validation

## Status

**Blocked on 2026-07-27 before deployment. No cluster resources were changed.**

The configured Kubernetes context is `admin@nuglab`, with API endpoint
`https://10.0.1.69:6443`. Both an authenticated `kubectl get` and an unauthenticated
`GET /livez` timed out. The host has a route through `192.168.10.1`, but the API
endpoint did not accept a TCP connection.

The configured manager image is also unavailable:

```text
ghcr.io/timblakely/llm-operator:latest: manifest unknown
```

Because the API inventory could not be read and the manager image cannot be
pulled, CRDs, RBAC, manager resources, and sample CRs were not applied. Backend
Deployments, cache-manager, and the proxy were not contacted or mutated.

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
- Transition mutation remains opt-in in manager code and manifests.

## Preconditions to resume

1. Restore network access to `10.0.1.69:6443` and confirm read access:

   ```bash
   kubectl config current-context
   kubectl get --raw=/livez
   kubectl get namespace llm
   ```

2. Build and publish the operator image. Replace `:latest` in
   `config/manager/manager.yaml` with the reviewed immutable image digest, then
   rerun `make check` and `make observation-preflight`.
3. Inventory the real resource names before applying sample CRs:

   ```bash
   kubectl -n llm get deployment,service,pod -o wide
   kubectl -n llm get configmap
   kubectl get crd | grep llm.cogito.dev || true
   ```

4. Review `deploymentRef`, `containerName`, `serviceRef`, port, model source,
   and revision in the sample CRs against that inventory. Do not apply a sample
   merely because its historical name resembles a live workload.

Publishing an image or changing cluster connectivity requires external action;
neither is performed by this repository validation.

## Observation procedure

Capture the transition-owned fields before applying operator resources:

```bash
kubectl -n llm get deployment -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\t"}{.spec.template.metadata.annotations.llm\.cogito\.dev/active-model}{"\t"}{range .spec.template.spec.containers[*]}{.name}{"="}{.args}{";"}{end}{"\n"}{end}' \
  > /tmp/llm-deployment-fields.before.txt
kubectl -n llm get pod -o wide > /tmp/llm-pods.before.txt
make observation-preflight
kubectl apply -k config/default
kubectl -n llm rollout status deployment/llm-operator-controller-manager
```

Immediately verify the live Deployment retained the safety flag:

```bash
kubectl -n llm get deployment llm-operator-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].args}'
```

After reviewing references, apply only the backend and model CRs that describe
existing workloads. Apply `LLMActiveModel` only after confirming transitions
are disabled in the live manager:

```bash
kubectl apply -f config/samples/llm_v1alpha1_llmbackend.yaml
kubectl apply -f config/samples/llm_v1alpha1_llmmodel.yaml
# Optional only when an SGLang workload already exists:
kubectl apply -f config/samples/llm_v1alpha1_sglang.yaml
kubectl apply -f config/samples/llm_v1alpha1_llmmodeloverlay.yaml
kubectl apply -f config/samples/llm_v1alpha1_llmactivemodel.yaml
```

Validate observed conditions and metadata:

```bash
kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay,llmactivemodel -o wide
kubectl -n llm get llmbackend,llmmodel,llmmodeloverlay,llmactivemodel -o yaml
kubectl -n llm logs deployment/llm-operator-controller-manager --since=1h
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
kubectl -n llm port-forward service/llm-operator-controller-manager-metrics 18081:8081
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

The safest rollback leaves CRDs and observations intact and stops only the
manager:

```bash
kubectl -n llm scale deployment/llm-operator-controller-manager --replicas=0
```

Alternatively, leave the manager running with `--enable-transitions=false`.
The proxy remains the serving controller in both cases. Do not delete CRDs as
an incident response step: that removes observation data and may interact with
model finalizers. Do not change backend, cache-manager, or proxy workloads as
part of operator rollback.

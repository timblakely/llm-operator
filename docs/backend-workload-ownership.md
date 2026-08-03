# Workload-owning `LLMBackend` redesign

## Decision

`LLMBackend` becomes the declarative owner of the Kubernetes objects that run
a serving runtime.  A backend must no longer identify a separately managed
Deployment or Service with `deploymentRef` and `serviceRef`.

The target ownership boundary is:

| Resource / concern | Owner |
| --- | --- |
| Backend Deployment, Service, selector, Pod template, mounts, resources, scheduling, and runtime image | `LLMBackend` |
| Artifact identity and model-specific runtime arguments | `LLMModel` |
| Which model is active, cache materialization, and one-active-backend transition ordering | `LLMActiveModel` controller |
| Operator manager, CRDs, RBAC, monitoring, and cache-manager infrastructure | Helm/Flux |

The controller creates a Deployment and Service named after its
`LLMBackend.metadata.name` by default. Explicit workload names are available
only to make a parallel migration safe. It sets an owner reference on both and
reconciles their full desired specification. It owns their lifecycle: deleting an
`LLMBackend` deletes its workload after the active-model safety check has
passed.

## API shape

The initial compatible API adds `spec.workload` to `v1alpha1` and deprecates
reference mode.  Exactly one mode is valid during migration.  `v1beta1` will
require `workload` and remove the three reference fields altogether.

```yaml
apiVersion: llm.cogito.dev/v1alpha1
kind: LLMBackend
metadata:
  name: deepseek-v4-flash
  namespace: llm
spec:
  type: llama-cpp
  workload:
    containerName: runtime
    service:
      name: deepseek-v4-flash-operator
      port: 8000
    deployment:
      name: deepseek-v4-flash-operator
      replicas: 0                 # operator controls 0/1 while switching
      strategy: Recreate
      podTemplate:
        metadata:
          labels: {}
          annotations: {}
        spec:
          automountServiceAccountToken: false
          runtimeClassName: nvidia
          nodeSelector:
            kubernetes.io/hostname: iggy
          containers:
            - name: runtime
              image: ghcr.io/ggml-org/llama.cpp:server-cuda13@sha256:...
              imagePullPolicy: IfNotPresent
              resources:
                requests: {cpu: "22", memory: 118Gi}
                limits: {cpu: "24", memory: 120Gi, nvidia.com/gpu: "2"}
              volumeMounts:
                - name: model-cache
                  mountPath: /models
                  subPath: gguf
          volumes:
            - name: model-cache
              persistentVolumeClaim:
                claimName: llm-huggingface-cache
```

`podTemplate` is a Kubernetes `PodTemplateSpec`, not an abbreviated custom
container schema.  This preserves standard Pod features (security context,
tolerations, affinity, init containers, projected volumes, and sidecars)
without forcing every future serving feature into this CRD.

The controller supplies and owns:

- Deployment/Service names, labels, and selectors;
- Deployment owner references and zero-replica inactive baseline;
- the runtime container's effective model arguments;
- active-model, switch-time, and chat-template annotations; and
- the ClusterIP Service port that exposes the named runtime container.

Admission rejects an attempt to set controller-reserved labels, duplicate
reference/workload modes, or runtime arguments in the pod template. It also requires
that `containerName` select exactly one container in `podTemplate`.

## Reconciliation contract

1. Reconcile the Service first, then the zero-replica Deployment.
2. Set controller ownership and reconcile the complete desired baseline.
3. Preserve only the explicitly controller-owned transition fields when
   reconciling a running backend; a backend spec update intentionally rolls
   out the workload.
4. `LLMActiveModel` resolves generated Deployment/Service names from the
   workload spec, never from a separately managed workload reference.
5. Before deleting a backend, reject deletion if it is selected by the active
   model.  A finalizer removes the generated objects only after the backend is
   inactive and scaled down.

The controller records health and replica observations against the generated
workload; transition code resolves its names from the backend workload spec.

## Migration

This is not a deletion-first cleanup.  The current Helm releases are needed
until the CR-created workloads have been accepted.

1. Release an operator that accepts both reference mode and `workload` mode,
   but does not create a workload for a backend still in reference mode.
2. Convert one inactive backend at a time into workload mode using distinct
   temporary `workload.deployment.name` and `workload.service.name` values.
   Compare the rendered Deployment and Service to the
   existing Helm objects, including GPU resources, PVC mounts, image digest,
   scheduling, and security settings.
3. Scale down and delete the corresponding Helm workload only after its
   replacement reports `Ready` and a proxy request has completed through it.
   Rename/adopt only with an explicit reviewed handoff; do not silently adopt
   a Helm-owned Deployment.
4. Migrate vLLM, Laguna, and DeepSeek independently.  Each currently has a
   materially different image, resource profile, and mount layout.
5. Remove the three backend HelmRelease directories and their Flux entries
   only after all live `LLMBackend` objects are workload mode and the operator
   has completed a transition among them.
6. Publish `v1beta1`, make `spec.workload` required, remove reference mode,
   and add a conversion/migration procedure before retiring `v1alpha1`.

The existing shared cache-manager remains a separate operator-adjacent
workload in this redesign.  It is infrastructure rather than a model-serving
backend and is not selected by `LLMActiveModel`.

## Acceptance gates

- Unit and envtest coverage proves creation, idempotence, drift correction,
  deletion protection, and transition ordering.
- Rendered CR-owned workloads match each Helm baseline before cutover.
- Flux no longer ignores fields on backend Deployments because it no longer
  owns them.
- At least one vLLM ↔ llama.cpp and Laguna ↔ DeepSeek transition succeeds
  using CR-owned workloads.
- The proxy receives an HTTP success from the intended active model after each
  cutover.

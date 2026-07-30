# llm-operator Helm chart

This chart installs only the llm-operator infrastructure: its four generated
CRDs, ServiceAccount and cluster-scoped RBAC, manager Deployment, metrics
Service, and PodDisruptionBudget. It deliberately does not contain sample or live
`LLMBackend`, `LLMModel`, `LLMModelOverlay`, or `LLMActiveModel` resources.

The intended Cogito artifact is:

```text
oci://ghcr.io/timblakely/charts/llm-operator
```

The image is digest-only. `image.digest` has no deployable default and must be
set to a reviewed `sha256:` digest. Model transitions default to disabled.
The manager currently watches CRs and referenced workloads cluster-wide, so the
chart uses a ClusterRoleBinding and should be installed only once per cluster.

## Validate and package

From the repository root:

```bash
make chart-check
make chart-package
```

`chart-check` verifies that the chart CRDs match the controller-generated
manifests, lints the chart, renders it with
[`ci/test-values.yaml`](ci/test-values.yaml), and asserts the observation-mode
and digest-pinning invariants. `chart-package` writes the versioned archive to
`dist/` after those checks pass. These targets bootstrap the pinned Helm
`v3.19.0` binary into the repository's ignored `bin/` directory; no global Helm
installation is required. Rendering and linting use the repository's pinned
Kubernetes capability version `1.35.0`.

## Install from source

```bash
make helm
bin/helm upgrade --install llm-operator charts/llm-operator \
  --namespace llm \
  --create-namespace \
  --set-string image.digest=sha256:<reviewed-image-digest>
```

For Cogito, publish the chart as OCI and let Flux install it. The Cogito
`HelmRelease` owns environment values and should use `CreateReplace` for CRDs on
install and upgrade. The namespace itself remains owned by Cogito, not by this
chart.

After authenticating Helm to GHCR, a maintainer can publish the validated chart
with:

```bash
make chart-push
```

This pushes `llm-operator:0.1.0` beneath
`oci://ghcr.io/timblakely/charts`. Publishing is intentionally separate from
ordinary validation.

For the matching digest-pinned manager image build and Cogito promotion steps,
see [Releasing the Manager Image and Helm Chart](../../docs/releasing.md).

## Important values

| Value | Default | Meaning |
|---|---:|---|
| `image.repository` | `ghcr.io/timblakely/llm-operator` | Manager image repository |
| `image.digest` | empty/required | Reviewed immutable manager image digest |
| `replicaCount` | `2` | Manager replicas |
| `leaderElection.enabled` | `true` | Serialize controller ownership across replicas |
| `transitions.enabled` | `false` | Permit backend Deployment mutation |
| `transitions.canaryModels` | `""` | Comma-separated canonical model allowlist when transitions are enabled |
| `admission.enabled` | `false` | Register validating admission handlers when webhook TLS/configuration is installed |
| `metrics.service.enabled` | `true` | Create the metrics Service |
| `cacheManager.url` | empty | Optional standalone cache-manager URL |
| `resources` | see values | Manager requests and limits |
| `podSecurityContext`, `securityContext` | hardened | Pod/container security configuration |
| `nodeSelector`, `tolerations`, `affinity` | empty | Pod scheduling controls |

Do not enable `transitions.enabled` during observation-mode validation.

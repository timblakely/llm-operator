# LLM proxy consolidation

## Decision

Move the `llm-proxy` source into this repository and package it as an optional
component of the existing `llm-operator` Helm chart.  This source and chart
consolidation **does not require a new CR**.

The proxy remains responsible for the public OpenAI-compatible API, catalog,
overlays, Hermes configuration, and CR-driven model handoff.  The operator
continues to own CR reconciliation and backend transitions.  In particular,
do not put the gateway server in `cmd/manager`, or run it as a manager
sidecar: manager leader-election or reconciliation restarts must not interrupt
inference or streaming responses.  One Helm release should render two
independent Deployments and ServiceAccounts: the controller manager and the
optional proxy.

`LLMGateway`/`LLMProxy` is explicitly not planned.  Chart ownership of the
proxy workload is sufficient and keeps cluster-specific policy out of the LLM
API.  Do not add a dormant gateway CR pre-emptively; reconsider only if a
concrete future requirement cannot be represented by Helm values and the
existing LLM resource contracts.

## Current contract

The Cogito `llm-switch` HTTPRoute sends all traffic to `Service/llm-proxy`
(`kubernetes/apps/llm/llm-proxy/app/proxy.yaml:145-183`).  Open WebUI uses
`https://llm-switch.${DOMAIN_NAME}/v1`, and Hermes calls
`http://llm-proxy.llm.svc.cluster.local:8080` during bootstrap.  Therefore a
move must preserve the public hostname, Service name/DNS, port, and route.

The current proxy binary exposes health, readiness, metrics, `/v1/models`,
`/v1/models/{id}`, Hermes configuration, and `/v1/*` inference
(`vllm/vllm-proxy/cmd/vllm-proxy/main.go:261-268` in Cogito).  It lists and
watches `LLMModel` and `LLMModelOverlay`, builds the catalog, applies overlay
defaults without overwriting client-supplied values, and reverse-proxies the
request.  Overlays are deliberately limited to POST `/v1/chat/completions`.

With `ENABLE_DEPLOYMENT_MUTATIONS=false`, the deployed proxy does not mutate
workloads.  For a request whose model is not active, it patches
`LLMActiveModel/default.spec.modelName`, waits for its status to become
`Stable`, discovers the selected backend, and forwards the original request.
That handoff is the contract to retain.

## Proposed chart and repository layout

Copy the proxy into a separate executable such as `cmd/llm-proxy` (or
`cmd/gateway`) with its supporting internal packages and tests.  It may use a
dedicated image target from this repository, or a reviewed multi-command
operator image; it must remain a separate Deployment.  Extend the existing
`charts/llm-operator` chart with an optional `proxy` component that renders
the proxy Deployment, Service, ServiceAccount, Role, RoleBinding,
ServiceMonitor, and (when enabled) HTTPRoute.

The chart should default `proxy.enabled` to `false` for a safe migration.
Cogito enables it only after the old GitOps proxy objects have been handed off
without overlap.  The chart values, not a CR, carry installation policy such
as image digest, resources, scheduling, rollout strategy, labels, public
hostname, Gateway API parent references, ServiceMonitor settings, and secret
references.  This is precisely the configuration that varies between Cogito
and another cluster.

Suggested value boundary:

```yaml
proxy:
  enabled: false
  image: {repository: ghcr.io/example/llm-proxy, digest: "sha256:..."}
  replicas: 1
  service: {name: llm-proxy, port: 8080}
  activeModelRef: default
  publicBaseURL: https://llm-switch.example.com/v1
  cacheManager: {url: http://cache-manager:8090}
  exposure:
    httpRoute:
      enabled: true
      hostnames: [llm-switch.example.com]
      parentRefs: [{name: envoy-internal, namespace: envoy-system, sectionName: https}]
```

Use a distinct proxy ServiceAccount and narrow Role: read model/overlay CRs,
get/watch the resolved backend identity, and get/patch only the configured
`LLMActiveModel`.  Do not inherit the manager's broader reconciliation RBAC.
ExternalSecret creation, DNS, certificate policy, and cluster-specific Grafana
dashboards can remain separately managed GitOps resources when they cannot be
expressed portably by the chart.

The proxy may continue to read the existing CRs directly.  Generic routing
must not depend on hard-coded runtime names: when that work is needed, extend
`LLMBackend` with a validated inference Service endpoint/reference and derive
the target from the selected model's backend.  That is a separate API design
and versioning change, not a prerequisite for consolidation.

### Metrics and monitoring

Metrics are a chart concern, not a gateway CR concern.  The chart should make
three independently enabled Prometheus scrape targets available:

| Target | Service endpoint | Chart behavior |
| --- | --- | --- |
| Operator manager | existing `:8081/metrics` Service | Optional manager `ServiceMonitor` for transition duration, failures, and model-switch counters. |
| Proxy | proxy Service `:8080/metrics` | Optional proxy `ServiceMonitor`; retain the existing 30-second Cogito scrape configuration and instrument request/routing metrics in the imported binary. |
| CR-owned backend | generated backend Service `/metrics` | One optional `ServiceMonitor` selecting Services with `llm.cogito.dev/backend` present. |

`LLMBackend` workload Services already receive the stable
`llm.cogito.dev/backend=<backend-name>` label.  Standardize their service port
name as `http` and scrape `/metrics` on that port.  The inference route and
Prometheus can therefore use the same Service and port, while target metadata
relabeling adds the backend name to scraped series.  Add runtime type only as
a stable Service label in a separate controller change; do not attach the
currently selected model to every runtime metric because that association
changes during transitions.  An inactive backend has zero endpoints and is
naturally not scraped; its desired and ready-replica state remains visible
through kube-state-metrics and the operator's transition metrics.

The runtime metric families remain runtime-specific: vLLM is metric-ready,
while SGLang requires `--enable-metrics` and llama.cpp requires `--metrics` in
their reviewed runtime arguments.  The chart must not claim that a backend is
observable until those arguments and `/metrics` response have been accepted.
Do not proxy backend metrics through the gateway or attempt to normalize their
metric names; scrape each backend directly and use the low-cardinality backend
target label in dashboards and alerts.

Suggested chart value boundary:

```yaml
metrics:
  serviceMonitor: {enabled: true, interval: 30s}
proxy:
  metrics:
    serviceMonitor: {enabled: true, interval: 30s}
backendMetrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    port: http
    path: /metrics
```

The ServiceMonitor CRD itself remains optional: installations without the
Prometheus Operator can create the same scrape jobs from their own monitoring
configuration.  The chart must expose labels/annotations so a platform's
existing discovery mechanism can select all three targets.

## Migration plan

1. Import the proxy code, its unit tests (including Hermes), module
   dependencies, and Docker build into this repository.  Preserve the current
   wire behavior and the read-only handoff; do not refactor it in the import.
2. Add an opt-in `proxy` section and templates to `charts/llm-operator`.
   Validate required digest, service/route names, active-model reference, and
   parent-reference shape in the values schema.  Keep manager and proxy RBAC,
   pods, Services, probes, and update strategies independent.
3. Split image targets if needed.  The current Cogito build also produces the
   standalone cache-manager from the proxy build context, so decide explicitly
   whether it moves too or receives its own build context.
4. Build and publish a digest-pinned proxy image, then migrate Cogito in a
   single reviewed Helm/GitOps handoff.  Retain its object names, endpoint,
   environment, route, and observability configuration; do not render a
   second proxy.  Update the Hermes bootstrap image reference in that rollout.
5. Render the chart and reconcile through Flux.  After acceptance, separately
   evaluate deletion of disabled deployment
   mutation code, proxy-owned `llm-model-status` metadata, and the proxy cache
   sweep.  Each is an independent compatibility change.

## Acceptance checklist

- [ ] Default chart render contains no proxy resources; an enabled render
  contains exactly one independently named proxy Deployment, ServiceAccount,
  Role/RoleBinding, Service, and optional exposure/monitoring resources.
- [ ] Chart schema rejects an enabled proxy without an immutable image digest,
  active-model reference, or the enabled HTTPRoute's required host/parent
  configuration.
- [ ] Enabled chart monitoring renders separate ServiceMonitors for manager,
  proxy, and Services carrying `llm.cogito.dev/backend`; all scrape `/metrics`
  through named Service ports and preserve installation-supplied labels.
- [ ] Each enabled backend returns Prometheus text from `/metrics`; SGLang and
  llama.cpp include their required metric-enablement flags.  An inactive
  backend yields no scrape target rather than a failed proxy scrape.
- [ ] Unit/contract tests cover CR catalog and cards, overlay-default
  precedence, streaming, unknown-model errors, timeout errors, and transition
  errors.
- [ ] Hermes configuration endpoint and Hermes bootstrap work with the new
  image.
- [ ] `/v1/models` remains compatible with Open WebUI and malformed legacy
  runtime metadata cannot make a valid CR catalog unavailable.
- [ ] A request for an inactive model patches only `LLMActiveModel/default`,
  reaches `Stable`, and is served by the selected backend.
- [ ] Existing public URL, `llm-proxy` in-cluster DNS, port, HTTPRoute,
  authentication/Secrets, and ServiceMonitor remain unchanged.
- [ ] Flux reports the intended source revision; the proxy pod uses the pinned
  digest and is ready.
- [ ] Open WebUI and Hermes both reach the proxy; an external/in-cluster proxy
  completion returns HTTP success with the intended model/fingerprint.
- [ ] Cache-manager ownership of sweep is recorded before removing the proxy
  sweep, and all externally visible catalog metadata has a CR-backed source
  before deleting `llm-model-status` writes.

## Concerns and corner cases

The current single replica with `Recreate` strategy produces a deliberate
outage during the image swap.  Change rollout strategy only after reviewing
streaming/session behavior and connection draining.

A proxy request may wait as long as the model transition timeout (currently
30 minutes).  Client cancellation can leave an activation in progress, and
concurrent model requests serialize indirectly through the singleton active
model; a later request can replace earlier intent.  Preserve and test these
semantics before attempting request coalescing or cancellation changes.

Readiness currently proves that the proxy registry loaded, not that an active
backend is healthy.  Keep backend health and `LLMActiveModel` status visible
in acceptance and alerting.  The current proxy also writes CR-source runtime
and model-card metadata to `ConfigMap/llm-model-status`, while the operator
already maintains CR status.  Audit actual `/v1/models` dependencies before
removing the ConfigMap path.  Likewise, the proxy's periodic cache-manager
`/v1/sweep` call overlaps the standalone cache-manager sweep interval; choose
one owner before removing either behavior.

## Source inventory

This report is based on a read-only Cogito review on 2026-08-07.  Key sources:

- `kubernetes/apps/llm/llm-proxy/app/proxy.yaml` — route, Deployment, RBAC,
  environment, and observability.
- `vllm/vllm-proxy/cmd/vllm-proxy/main.go` — gateway API, CR catalog,
  read-only handoff, metadata, and sweep behavior.
- `kubernetes/apps/llm/open-webui/app/helmrelease.yaml` and
  `kubernetes/apps/llm/hermes/app/helmrelease.yaml` — public and in-cluster
  consumers.
- `kubernetes/apps/llm/cache-manager/app/helmrelease.yaml` — independent
  sweep schedule.
- This repository's `api/cogito.dev/v1alpha1` and `internal/controller` —
  current CR and transition contracts.

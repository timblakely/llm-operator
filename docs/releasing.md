# Releasing the Manager Image and Helm Chart

Cogito deploys the manager image by immutable digest. Do not use `latest` or a
mutable image tag in the Flux `HelmRelease`.

## Prerequisites

- Push permission for `ghcr.io/timblakely/llm-operator` and
  `ghcr.io/timblakely/charts`.
- Docker authenticated to GHCR (`docker login ghcr.io`).
- A clean local validation run.

## Build and publish the manager image

Choose a release version shared by the image tag and chart version. For example:

```bash
export VERSION=0.1.0
make check
make docker-build IMG=ghcr.io/timblakely/llm-operator:${VERSION}
make docker-push IMG=ghcr.io/timblakely/llm-operator:${VERSION}
```

Retrieve the pushed image digest and record the `sha256:...` value:

```bash
docker buildx imagetools inspect ghcr.io/timblakely/llm-operator:${VERSION}
```

Confirm that the digest resolves to the expected image before promoting it. The
Helm chart renders the image as:

```text
ghcr.io/timblakely/llm-operator@sha256:<reviewed-digest>
```

## Package and publish the Helm chart

The chart version is currently declared in
[`charts/llm-operator/Chart.yaml`](../charts/llm-operator/Chart.yaml). Bump it
before releasing a new chart revision, then run:

```bash
make chart-package
bin/helm registry login ghcr.io
make chart-push
```

This publishes `llm-operator:<chart-version>` to:

```text
oci://ghcr.io/timblakely/charts/llm-operator
```

## Promote to Cogito observation mode

1. Commit the reviewed chart OCI digest to Cogito's `OCIRepository`.
2. Commit the reviewed image digest directly in Cogito's HelmRelease values.
   An image digest is deployment metadata, not a credential, and Git provides
   the promotion audit trail.
3. Commit the matching HelmRelease update with `transitions.enabled=false`.
4. Reconcile Flux and verify the live Deployment uses the expected digest and
   contains `--enable-transitions=false`.

Follow [the observation validation runbook](../plans/observation_validation.md)
for the full safety checks and rollback procedure.

# LLM Operator

A Kubernetes Operator for managing LLM model serving backends (vLLM and llama.cpp).

## Overview

The LLM Operator replaces the monolithic ConfigMap + vllm-proxy architecture with proper Kubernetes CRDs, a controller manager with leader election, and a decoupled API proxy.

## CRDs

| Kind | Description | Short Name |
|------|-------------|------------|
| `LLMModel` | A model definition with serving config | `llmm` |
| `LLMModelOverlay` | Virtual model with request defaults | `llmo` |
| `LLMActiveModel` | Singleton tracking the active model | `llma` |
| `LLMBackend` | Serving backend deployment registry | `llmb` |

## Quick Start

### Build

```bash
make build
```

### Run locally

```bash
make run
```

### Deploy CRDs

```bash
make deploy-crd
```

### Apply samples

```bash
make samples-apply
```

### Full deploy

```bash
make deploy
```

## Architecture

```
Client → Ingress → LLM Proxy → Backend Deployment (vllm or llama-cpp)
                    ↑
             Reads LLMModel, LLMModelOverlay, LLMActiveModel CRDs
```

### Controllers

- **LLMModelController**: Validates model specs, manages finalizers, updates status
- **LLMActiveModelController**: Orchestrates model transitions (scale down → cache → patch → scale up → health check)
- **LLMModelOverlayController**: Validates overlays against base models
- **LLMBackendController**: Monitors backend health and deployment status

### Cache Manager

The cache-manager runs as a standalone Deployment. The controller calls it during model transitions to ensure artifacts are in the hot cache.

## Migration from ConfigMaps

See `OPERATOR_PLAN.md` for the phased migration plan.

### Phase 1: CRDs + Dual-mode proxy
### Phase 2: Controller replaces proxy logic
### Phase 3: Full operator + polish

## Project Structure

```
llm-operator/
├── cmd/manager/main.go              # Controller manager entrypoint
├── api/cogito.dev/v1alpha1/         # CRD types
├── internal/controller/              # Controllers
├── internal/cache/                   # Cache-manager HTTP client
├── config/crd/                      # Generated CRD manifests
├── config/rbac/                     # Generated RBAC
├── config/manager/                  # Controller manager Deployment
├── config/samples/                  # Sample CRD instances
├── Dockerfile
├── Makefile
└── go.mod
```

## License

Apache 2.0
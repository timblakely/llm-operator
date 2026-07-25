# Adding a Compiled-in Backend

Backend drivers are compiled into the operator. Controllers must use the
`backend.Driver` contract and must not branch on a runtime type.

## Driver contract

Every driver declares these capabilities:

| Capability | Meaning |
|---|---|
| OpenAI model discovery | The runtime provides an OpenAI-compatible model list. |
| Tool-call parser | `spec.serving.toolCallParser` can be translated safely. |
| Reasoning parser | `spec.serving.reasoningParser` can be translated safely. |
| Metrics | The runtime has a Prometheus metrics endpoint. Some runtimes require an enablement flag. |
| Cache format | Artifact layout, currently `huggingface-hub` or `gguf`. |
| Health path | Runtime-specific readiness endpoint used by both backend observation and transitions. |

The interface also owns argument validation and construction, health semantics,
model discovery, runtime metadata collection, and cache-manager request
construction. Unsupported portable fields must return a validation error; the
model controller exposes that error through its `ModelConfigured` condition.

## Extension procedure

1. Add the new value to `BackendType`, its validation markers, and the generated
   CRD enum.
2. Implement a `backend.Driver`. Prefer the shared `runtimeDriver` only when its
   OpenAI discovery response and argument conventions actually match.
3. Declare the complete capability set. Do not advertise parser, metrics, or
   cache behavior that the driver does not implement.
4. Reserve every controller-injected argument in `ValidateArgs`, then build the
   effective launch arguments without mutating the `LLMModel`.
5. Implement runtime-specific health, metadata parsing, and cache construction
   inside the driver package. Do not add runtime switches to controllers.
6. Register the driver in `DefaultRegistry`.
7. Add contract tests covering capabilities, launch arguments, validation,
   health path/status handling, model discovery, metadata, and cache requests.
8. Add representative `LLMBackend` and `LLMModel` samples.
9. Run `make manifests generate` and `make check`. Generated-file drift and CRD
   schema validation must remain clean.

## Current capabilities

| Backend | Discovery | Tool parser | Reasoning parser | Metrics | Cache | Health |
|---|---:|---:|---:|---:|---|---|
| vLLM | yes | yes | yes | yes | `huggingface-hub` | `/health` |
| SGLang | yes | yes | yes | yes, with `--enable-metrics` | `huggingface-hub` | `/health_generate` |
| llama.cpp | yes | no | no | yes, with `--metrics` | `gguf` | `/health` |

All three drivers collect model IDs from `/v1/models`. Runtime metadata records
portable launch arguments and the runtime's concurrency flag. vLLM additionally
parses its cache configuration metric into `status.runtimeMetadata.kvCache`.
Other runtime metric families remain available to Prometheus without being
misrepresented as vLLM cache metadata.

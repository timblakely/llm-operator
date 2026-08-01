# M9 — Managed Serving Templates

## Status

**Planned — documentation and design only.** No API, controller, chart, or
Cogito workload change is authorized by this plan alone.

## Objective

Make server-side chat templates an explicit, reviewed, digest-pinned part of a
model's serving configuration. The feature must support vLLM now and provide a
portable operator contract for llama.cpp and SGLang where their runtime
capabilities have been verified.

The immediate use case is the Qwen 3.6 tool-calling regression exposed by
Pi's large tool schema. The live Qwen model uses its stock template and
`qwen3_coder`; the proposed Qwen replacement retains that parser. This is a
backend/template experiment, not a Pi-only workaround and not a reason to
weaken vLLM's request-template safety.

## Design Decisions

1. **No new CRD for M9.** Add an optional model-level template reference to
   `LLMModel.spec.serving`; a template changes the runtime's behavior for all
   callers of a model, so `LLMModelOverlay` is not the ownership layer.
2. **GitOps owns bytes.** Cogito vendors reviewed template files into a
   ConfigMap. The operator never fetches a template from an upstream URL while
   reconciling.
3. **References are immutable in effect.** The model records a same-namespace
   ConfigMap name/key and SHA-256 digest. Reconciliation fails visibly when
   the referenced bytes do not match that digest.
4. **Drivers translate, controllers coordinate.** The portable API describes
   a template; each backend driver produces its native mount and launch
   arguments. Unsupported backends are rejected rather than silently ignoring
   the reference.
5. **Profiles are deferred.** Add an `LLMServingProfile` CRD only if repeated
   model configurations prove that a shared template/parser/reasoning bundle
   is needed. A single template reference is the maintainable starting point.

## Proposed API

```yaml
apiVersion: llm.cogito.dev/v1alpha1
kind: LLMModel
spec:
  serving:
    backend: vllm
    toolCallParser: qwen3_coder
    chatTemplate:
      configMapKeyRef:
        name: qwen-fixed-chat-template
        key: chat_template.jinja
      sha256: <lowercase-hex-content-digest>
```

`chatTemplate` is optional. Omitting it preserves the model/runtime default.
`configMapKeyRef` is namespaced with the `LLMModel`; cross-namespace template
references are deliberately out of scope.

## Stages and Exit Criteria

### M9.0 — Contract and ownership design

1. Define Go API types, validation rules, and generated CRD schema for the
   optional reference and digest.
2. Specify controller ownership of the injected volume, mount, launch
   argument, annotation, and their cleanup when a model changes templates.
3. Define status/runtime metadata fields that expose the resolved
   ConfigMap/key/digest without copying template contents.
4. Decide the exact behavior for a missing key, unreadable ConfigMap, digest
   mismatch, and a backend that does not support templates.

**Exit:** reviewed API and ownership contract; no runtime behavior changed.

### M9.1 — Operator API, validation, and reconciliation

1. Add the API types, CRD generation, CEL/static validation where applicable,
   and controller-side ConfigMap/key/digest validation.
2. Watch referenced ConfigMaps and requeue only the models that reference
   them.
3. Set clear conditions for `TemplateResolved`, `TemplateDigestMismatch`, and
   `TemplateUnsupported`; preserve the last stable runtime configuration on a
   failed update.
4. Add unit/envtest coverage for absent, valid, missing, changed, and
   mismatched templates.

**Exit:** a valid template reference is observable and invalid references fail
predictably without mutating a running backend.

### M9.2 — Backend-driver and workload integration

1. Extend the driver contract with template mount/argument generation and
   cleanup semantics.
2. Implement vLLM translation to `--chat-template <mounted-file>`.
3. Implement llama.cpp translation to `--jinja --chat-template-file
   <mounted-file>` after verifying the deployed binary's supported flags.
4. Add SGLang only after its current server-side template mechanism is
   verified; otherwise reject it explicitly.
5. Add render and fake-client transition tests proving no duplicate flags,
   stable volume names, removal on model change, and sidecar preservation.

**Exit:** drivers produce deterministic, backend-correct Pod mutations with
complete cleanup.

### M9.3 — Qwen GitOps validation in Cogito

1. Vendor the reviewed Qwen fixed Jinja template with upstream URL, revision,
   license, and content digest recorded in Git.
2. Create the template ConfigMap and bind it only to the Qwen `LLMModel`.
3. Keep `toolCallParser: qwen3_coder`; do not change parsers concurrently.
4. Reconcile through Flux and verify the active pod uses the expected mounted
   file, digest, and vLLM launch argument.
5. Replay the captured Pi request and validate an actual Pi tool loop yields
   structured OpenAI `tool_calls`, including a post-tool response.

**Exit:** Qwen returns structured calls for the regression payload without
using request-level template overrides or a Pi-specific `tool_choice` shim.

### M9.4 — Generalization and operational hardening

1. Add a Gemma template regression fixture before applying any Gemma template
   update.
2. Document the GitOps authoring, provenance, validation, rollback, and
   incident-diagnosis workflow.
3. Add a runtime/container integration test covering mounted template changes
   and a failed digest validation.
4. Reassess whether repeated template/parser/reasoning bundles justify an
   `LLMServingProfile` CRD.

**Exit:** templates are a tested, documented portable feature; a profile CRD
remains a data-driven decision rather than an assumption.

## Non-goals

- Fetching or trusting client-supplied templates (`--trust-request-chat-template`
  remains disabled).
- Treating `LLMModelOverlay.requestDefaults` as a mechanism to change a
  backend's mounted template.
- Changing Qwen parser and template in the same experiment.
- Introducing an `LLMServingProfile` CRD before multiple real model families
  need it.

## Related Records

- [remaining_work.md](remaining_work.md): historical M0–M8 migration record.
- [observation_validation.md](observation_validation.md): historical
  observation-mode validation evidence.
- [froggeric/Qwen-Fixed-Chat-Templates](https://huggingface.co/froggeric/Qwen-Fixed-Chat-Templates): candidate Qwen template source; vendor and pin it before use.

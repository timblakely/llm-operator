package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStatusDataKeyEncodesCRSourceForConfigMap(t *testing.T) {
	got := statusDataKey("crd/gemma-4-31b", ".runtime_metadata.json")
	if got != "crd__gemma-4-31b.runtime_metadata.json" {
		t.Fatalf("status key = %q", got)
	}
	if strings.Contains(got, "/") {
		t.Fatalf("ConfigMap data key contains slash: %q", got)
	}
}

func TestSyncActiveDeploymentReconcilesExternalModelChange(t *testing.T) {
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{activeModelAnno: "gemma"},
		}}},
	})
	p := &proxy{
		client: client, namespace: "home-infra", deployment: "llm-vllm", active: "qwen",
		registry: registry{models: map[string]modelConfig{"gemma": {Name: "gemma"}, "qwen": {Name: "qwen"}}},
	}
	if err := p.syncActiveDeployment(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.active != "gemma" {
		t.Fatalf("active model = %q, want gemma", p.active)
	}
}

func TestReconcileActiveDeploymentCancelsStaleTransition(t *testing.T) {
	replicas := int32(1)
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}}},
		},
	})
	canceled := make(chan struct{})
	p := &proxy{
		client:           client,
		namespace:        "home-infra",
		deployment:       "llm-vllm",
		container:        "vllm",
		active:           "gemma",
		transitioning:    true,
		transitionCancel: func() { close(canceled) },
		registry: registry{models: map[string]modelConfig{
			"gemma": {Name: "gemma", ModelSource: "google/gemma"},
		}},
	}

	p.reconcileActiveDeployment(slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stale transition was not canceled")
	}
	if !p.reconcilePending {
		t.Fatal("updated active model was not queued for reconciliation")
	}
}

func TestReadOnlyTransitionsDoNotReconcileDeployments(t *testing.T) {
	zero := int32(0)
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm"}}}},
		},
	})
	p := &proxy{
		client:              client,
		namespace:           "home-infra",
		container:           "vllm",
		active:              "gemma",
		readOnlyTransitions: true,
		backends:            map[string]backendConfig{"vllm": {Name: "vllm", Deployment: "llm-vllm", Container: "vllm"}},
		registry:            registry{models: map[string]modelConfig{"gemma": {Name: "gemma", Backend: "vllm", ModelSource: "google/gemma"}}},
	}

	p.reconcileActiveDeployment(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("read-only reconciliation made Kubernetes calls: %#v", actions)
	}
}

func TestSyncActiveDeploymentPreservesInFlightTransition(t *testing.T) {
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{activeModelAnno: "gemma"},
		}}},
	})
	p := &proxy{
		client: client, namespace: "home-infra", deployment: "llm-vllm", active: "qwen", transitioning: true,
		registry: registry{models: map[string]modelConfig{"gemma": {Name: "gemma"}, "qwen": {Name: "qwen"}}},
	}
	if err := p.syncActiveDeployment(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.active != "qwen" {
		t.Fatalf("active model = %q, want qwen during transition", p.active)
	}
}

func TestSyncActiveDeploymentSelectsActiveLagunaBackend(t *testing.T) {
	zero, one := int32(0), int32(1)
	lagunaURL, _ := url.Parse("http://laguna:8000")
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"}, Spec: appsv1.DeploymentSpec{Replicas: &zero}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "laguna", Namespace: "home-infra"}, Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{activeModelAnno: "laguna"}}}}},
	)
	p := &proxy{
		client: client, namespace: "home-infra", active: "gemma",
		backends: map[string]backendConfig{
			"vllm":      {Name: "vllm", Deployment: "llm-vllm"},
			"llama-cpp": {Name: "llama-cpp", Deployment: "laguna", URL: lagunaURL},
		},
		registry: registry{models: map[string]modelConfig{"laguna": {Name: "laguna", Backend: "llama-cpp"}}},
	}
	if err := p.syncActiveDeployment(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.active != "laguna" || p.backendName != "llama-cpp" || p.backend.String() != lagunaURL.String() {
		t.Fatalf("unexpected active backend: model=%q backend=%q url=%v", p.active, p.backendName, p.backend)
	}
}

func TestSyncActiveDeploymentRejectsMultipleBackends(t *testing.T) {
	one := int32(1)
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"}, Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{activeModelAnno: "gemma"}}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "laguna", Namespace: "home-infra"}, Spec: appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{activeModelAnno: "laguna"}}}}},
	)
	p := &proxy{client: client, namespace: "home-infra", backends: map[string]backendConfig{"vllm": {Name: "vllm", Deployment: "llm-vllm"}, "llama-cpp": {Name: "llama-cpp", Deployment: "laguna"}}}
	if err := p.syncActiveDeployment(context.Background()); err == nil || !strings.Contains(err.Error(), "multiple LLM backends") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTransitionKeepsCurrentBackendRunningWhenTargetIsMissing(t *testing.T) {
	one := int32(1)
	client := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
	})
	p := &proxy{
		client: client, namespace: "home-infra", backendName: "vllm", transitionLimit: time.Second,
		backends: map[string]backendConfig{
			"vllm":      {Name: "vllm", Deployment: "llm-vllm", Container: "vllm"},
			"llama-cpp": {Name: "llama-cpp", Deployment: "laguna", Container: "laguna"},
		},
	}
	if err := p.transition(context.Background(), modelConfig{Name: "laguna", Backend: "llama-cpp"}); err == nil || !strings.Contains(err.Error(), "get llama-cpp backend Deployment") {
		t.Fatalf("unexpected error: %v", err)
	}
	deployment, err := client.AppsV1().Deployments("home-infra").Get(context.Background(), "llm-vllm", metav1.GetOptions{})
	if err != nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("current backend was changed after failed preflight: deployment=%#v err=%v", deployment, err)
	}
}

func TestEnsureActiveCancelsInFlightTransitionForRequestedModel(t *testing.T) {
	canceled := make(chan struct{})
	p := &proxy{
		active:          "qwen",
		transitioning:   true,
		transitionModel: "gemma",
		transitionCancel: func() {
			close(canceled)
		},
		registry: registry{models: map[string]modelConfig{
			"gemma": {Name: "gemma"},
			"qwen":  {Name: "qwen"},
		}},
	}

	err := p.ensureActive(context.Background(), "qwen")
	if !errors.Is(err, errTransitioning) {
		t.Fatalf("ensureActive error = %v, want %v", err, errTransitioning)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight transition was not canceled")
	}
	if !p.reconcilePending {
		t.Fatal("requested model was not queued for reconciliation")
	}
}

func TestReadOnlyTransitionsRequestOperatorHandoffForInactiveModels(t *testing.T) {
	active := activeModelObject(activeModelName, "gemma")
	p := &proxy{
		readOnlyTransitions: true,
		namespace:           "home-infra",
		dynamic:             newLLMDynamicClient(active),
	}
	if err := p.requestOperatorTransition(context.Background(), "qwen"); err != nil {
		t.Fatal(err)
	}
	got, err := p.dynamic.Resource(activeModelGVR).Namespace("home-infra").Get(context.Background(), activeModelName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	modelName, _, err := unstructured.NestedString(got.Object, "spec", "modelName")
	if err != nil || modelName != "qwen" {
		t.Fatalf("requested active model = %q, err = %v", modelName, err)
	}
}

func TestWaitForOperatorTransitionReturnsWhenStable(t *testing.T) {
	active := activeModelObject(activeModelName, "qwen")
	active.Object["status"] = map[string]any{"modelName": "qwen", "phase": "Stable"}
	p := &proxy{
		namespace:       "home-infra",
		dynamic:         newLLMDynamicClient(active),
		transitionLimit: time.Second,
	}
	if err := p.waitForOperatorTransition(context.Background(), "qwen"); err != nil {
		t.Fatal(err)
	}
}

func llmModelObject(name, modelName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "llm.cogito.dev/v1alpha1", "kind": "LLMModel",
		"metadata": map[string]any{"name": name, "namespace": "home-infra", "creationTimestamp": "2026-07-29T00:00:00Z"},
		"spec": map[string]any{
			"model":   map[string]any{"name": modelName, "source": modelName, "revision": "52f3f65bc7a02d555763bc923bd1d9094898219d"},
			"serving": map[string]any{"backend": "vllm", "displayName": "Gemma", "maxModelLen": int64(32768), "args": []any{"--host", "0.0.0.0"}},
		},
	}}
}

func llmOverlayObject(name, base string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "llm.cogito.dev/v1alpha1", "kind": "LLMModelOverlay",
		"metadata": map[string]any{"name": name, "namespace": "home-infra", "creationTimestamp": "2026-07-29T00:00:00Z"},
		"spec":     map[string]any{"displayName": "Gemma Agentic", "baseModel": base, "requestDefaults": map[string]any{"temperature": 0.7}},
	}}
}

func activeModelObject(name, modelName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "llm.cogito.dev/v1alpha1", "kind": "LLMActiveModel",
		"metadata": map[string]any{"name": name, "namespace": "home-infra"},
		"spec":     map[string]any{"modelName": modelName},
	}}
}

func newLLMDynamicClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		llmModelGVR:    "LLMModelList",
		llmOverlayGVR:  "LLMModelOverlayList",
		activeModelGVR: "LLMActiveModelList",
	})
	for _, object := range objects {
		item := object.(*unstructured.Unstructured)
		gvr := llmModelGVR
		if item.GetKind() == "LLMModelOverlay" {
			gvr = llmOverlayGVR
		} else if item.GetKind() == "LLMActiveModel" {
			gvr = activeModelGVR
		}
		if _, err := client.Resource(gvr).Namespace(item.GetNamespace()).Create(context.Background(), item, metav1.CreateOptions{}); err != nil {
			panic(err)
		}
	}
	return client
}

func TestRefreshReadsCRDModelsAndOverlays(t *testing.T) {
	model := llmModelObject("gemma", "google/gemma")
	overlay := llmOverlayObject("gemma-agentic", "google/gemma")
	status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: modelStatusName, Namespace: "home-infra"}, Data: map[string]string{
		"crd__gemma.runtime_metadata.json":    `{"schema_version":1,"source":"runtime"}`,
		"crd__gemma.model_card_metadata.json": `{"source":"catalog"}`,
	}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "llm-vllm", Namespace: "home-infra"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{activeModelAnno: "google/gemma"}}}}}
	p := &proxy{client: fake.NewSimpleClientset(status, deployment), dynamic: newLLMDynamicClient(model, overlay), namespace: "home-infra", deployment: "llm-vllm"}
	if err := p.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, ok := p.registry.models["google/gemma"]
	if !ok || cfg.Source != "crd/gemma" {
		t.Fatalf("CRD model not loaded: %#v", p.registry.models)
	}
	if string(cfg.Runtime) != `{"schema_version":1,"source":"runtime"}` || string(cfg.Fallback) != `{"source":"catalog"}` {
		t.Fatalf("CRD status metadata was not attached: %#v", cfg)
	}
	if _, ok := p.registry.overlays["gemma-agentic"]; !ok {
		t.Fatalf("CRD overlay not loaded: %#v", p.registry.overlays)
	}
	recorder := httptest.NewRecorder()
	p.models(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"config_source":"crd/gemma"`) || !strings.Contains(recorder.Body.String(), `"id":"gemma-agentic"`) {
		t.Fatalf("unexpected CRD catalog: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestParseLLMModelPreservesArtifactSize(t *testing.T) {
	model := llmModelObject("laguna", "poolside/Laguna-S-2.1")
	_ = unstructured.SetNestedField(model.Object, "llama-cpp", "spec", "serving", "backend")
	_ = unstructured.SetNestedStringSlice(model.Object, []string{"--host", "0.0.0.0"}, "spec", "serving", "args")
	_ = unstructured.SetNestedField(model.Object, "poolside/Laguna-S-2.1-GGUF", "spec", "model", "artifactRepository")
	_ = unstructured.SetNestedField(model.Object, "60Gi", "spec", "artifact", "expectedSize")
	_ = unstructured.SetNestedStringSlice(model.Object, []string{"laguna.gguf"}, "spec", "artifact", "files")
	cfg, err := parseLLMModel(*model)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Kind != "huggingface-files" || cfg.Cache.Size != 60*1024*1024*1024 || len(cfg.Cache.Files) != 1 {
		t.Fatalf("artifact cache = %#v", cfg.Cache)
	}
}

func TestApplyOverlayUsesClientOverridesAndBaseModel(t *testing.T) {
	overlay := overlayConfig{BaseModel: "gemma", RequestDefaults: json.RawMessage(`{"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":true},"temperature":0.7}`)}
	body, err := applyOverlay([]byte(`{"model":"gemma4-agentic","temperature":0.2,"chat_template_kwargs":{"enable_thinking":false}}`), overlay)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["model"] != "gemma" || got["temperature"] != 0.2 {
		t.Fatalf("unexpected overlay request: %#v", got)
	}
	kwargs := got["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false || kwargs["preserve_thinking"] != true {
		t.Fatalf("unexpected template kwargs: %#v", kwargs)
	}
}

func TestOverlayModelCatalog(t *testing.T) {
	p := &proxy{registry: registry{
		models:   map[string]modelConfig{"gemma": {Name: "gemma", MaxModelLen: 32768, Created: time.Unix(1, 0)}},
		overlays: map[string]overlayConfig{"gemma4-agentic": {Name: "gemma4-agentic", BaseModel: "gemma", Created: time.Unix(2, 0)}},
	}}
	recorder := httptest.NewRecorder()
	p.models(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"gemma4-agentic"`) || !strings.Contains(recorder.Body.String(), `"base_model":"gemma"`) {
		t.Fatalf("unexpected catalog: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestInferenceOverlayForwardsBaseModel(t *testing.T) {
	var received map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{
		backend: backendURL,
		active:  "gemma",
		maxBody: 1 << 20,
		registry: registry{
			models: map[string]modelConfig{"gemma": {Name: "gemma"}},
			overlays: map[string]overlayConfig{"gemma4-agentic": {
				Name: "gemma4-agentic", BaseModel: "gemma", RequestDefaults: json.RawMessage(`{"chat_template_kwargs":{"enable_thinking":true,"preserve_thinking":true}}`),
			}},
		},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gemma4-agentic","messages":[],"chat_template_kwargs":{"enable_thinking":false}}`))
	p.inference(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", recorder.Code, recorder.Body.String())
	}
	if received["model"] != "gemma" {
		t.Fatalf("forwarded model = %#v, want gemma", received["model"])
	}
	kwargs := received["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false || kwargs["preserve_thinking"] != true {
		t.Fatalf("forwarded kwargs = %#v", kwargs)
	}
}

func TestDeploymentNeedsActivation(t *testing.T) {
	replicas := int32(1)
	cfg := modelConfig{Name: "gemma", ModelSource: "google/gemma", Args: []string{"--host", "0.0.0.0"}}
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Replicas: &replicas,
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "vllm", Args: effectiveVLLMArgs(cfg),
		}}}},
	}}
	if deploymentNeedsActivation(deployment, "vllm", cfg) {
		t.Fatal("matching one-replica Deployment should not require activation")
	}

	zero := int32(0)
	deployment.Spec.Replicas = &zero
	if !deploymentNeedsActivation(deployment, "vllm", cfg) {
		t.Fatal("zero-replica Deployment should require activation")
	}

	deployment.Spec.Replicas = &replicas
	deployment.Spec.Template.Spec.Containers[0].Args = []string{"--model", "wrong"}
	if !deploymentNeedsActivation(deployment, "vllm", cfg) {
		t.Fatal("Deployment with stale arguments should require activation")
	}
}

func TestModelContextLength(t *testing.T) {
	length, ok := modelContextLength(map[string]any{"text_config": map[string]any{"max_position_embeddings": json.Number("131072")}})
	if !ok || length != 131072 {
		t.Fatalf("got (%d, %t), want (131072, true)", length, ok)
	}
}

func TestWriteGeneratedConfig(t *testing.T) {
	var output bytes.Buffer
	if err := writeGeneratedConfig(&output, "NousResearch/Hermes-3-Llama-3.1-8B", 131072, 65536, map[string]any{"model_max_context": 131072}); err != nil {
		t.Fatal(err)
	}
	config := output.String()
	for _, want := range []string{"kind: LLMModel", "name: 'NousResearch/Hermes-3-Llama-3.1-8B'", "maxModelLen: 65536", "source: 'NousResearch/Hermes-3-Llama-3.1-8B'"} {
		if !strings.Contains(config, want) {
			t.Fatalf("generated config missing %q:\n%s", want, config)
		}
	}
}

func TestCacheConfigInfo(t *testing.T) {
	metrics := "# HELP vllm:cache_config_info Information\n" +
		"vllm:cache_config_info{block_size=\"16\",cache_dtype=\"fp8_e5m2\",gpu_memory_utilization=\"0.92\",num_gpu_blocks=\"4096\"} 1\n"
	cache := cacheConfigInfo(metrics)
	if cache["block_size"] != "16" || cache["num_gpu_blocks"] != "4096" {
		t.Fatalf("unexpected cache metadata: %#v", cache)
	}
}

func TestArtifactManifestDetectsPayloadChanges(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "payload", "files")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, "model.gguf"), []byte("original model bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("model.gguf", filepath.Join(payload, "current.gguf")); err != nil {
		t.Fatal(err)
	}
	spec := cacheSpec{Kind: "huggingface-files", RepoID: "example/model", Revision: "0123456789012345678901234567890123456789", Size: 1024, Files: []string{"model.gguf"}}
	if err := writeArtifactManifest(dir, spec); err != nil {
		t.Fatal(err)
	}
	if err := writeComplete(dir); err != nil {
		t.Fatal(err)
	}
	if !validArtifact(dir, spec) {
		t.Fatal("fresh artifact should be valid")
	}
	if err := os.WriteFile(filepath.Join(payload, "model.gguf"), []byte("corrupted bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if validArtifact(dir, spec) {
		t.Fatal("corrupted artifact was accepted")
	}
}

func TestModelCardPrefersRuntimeMetadata(t *testing.T) {
	cfg := modelConfig{Name: "model", Created: time.Unix(1, 0), Fallback: json.RawMessage(`{"source":"huggingface"}`), Runtime: json.RawMessage(`{"source":"vllm_runtime"}`)}
	metadata, ok := modelCard(cfg)["metadata"].(map[string]any)
	if !ok || metadata["source"] != "vllm_runtime" {
		t.Fatalf("runtime metadata was not selected: %#v", modelCard(cfg))
	}
}

func TestHermesConfigEndpointUsesActiveModel(t *testing.T) {
	p := &proxy{registry: registry{models: map[string]modelConfig{
		"gemma": {Name: "gemma", MaxModelLen: 32768, Created: time.Unix(1, 0)},
		"qwen":  {Name: "qwen", MaxModelLen: 65536, Created: time.Unix(2, 0)},
	}}, active: "gemma", publicBaseURL: "https://llm.example/v1"}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/vllm-proxy/config/hermes-agent", nil)
	p.hermesConfig(recorder, request)
	for _, want := range []string{`"default":"gemma"`, `"provider":"custom:llm-proxy"`, `"name":"llm-proxy"`, `"gemma":{"context_length":32768}`, `"qwen":{"context_length":65536}`} {
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("unexpected config endpoint response: %d %s", recorder.Code, recorder.Body.String())
		}
	}
	if strings.Contains(recorder.Body.String(), `"api_key"`) {
		t.Fatalf("unexpected config endpoint response: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestWriteHermesConfigPreservesOtherSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("agent:\n  max_turns: 20\ncustom_providers:\n  - name: other\n    base_url: https://other.example/v1\n  - name: llm-proxy\n    base_url: https://old.example/v1\n    models:\n      stale:\n        context_length: 1\nmodel:\n  aliases:\n    fav: custom:llm-proxy:gemma\n  provider: custom:llm-proxy\n  base_url: https://old.example/v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	remote := hermesConfigPayload{
		Model: hermesModelConfig{Default: "gemma", Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{Name: "llm-proxy", BaseURL: "https://llm.example/v1", APIMode: "chat_completions", Models: map[string]hermesCustomProviderModel{
			"gemma": {ContextLength: 32768}, "qwen": {ContextLength: 65536},
		}}},
	}
	if err := writeHermesConfig(path, remote); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"max_turns: 20", "default: gemma", "provider: custom:llm-proxy", "base_url: https://llm.example/v1", "name: other", "name: llm-proxy", "context_length: 32768", "context_length: 65536", "fav: custom:llm-proxy:gemma"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("updated config missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "stale") || strings.Contains(string(body), "https://old.example") {
		t.Fatalf("stale proxy configuration remained:\n%s", body)
	}
}

func TestWriteHermesConfigPreservesOpenAIModelSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "model:\n  provider: openai-api\n  default: gpt-5\n  base_url: https://api.openai.example/v1\ncustom_providers:\n  - name: llm-proxy\n    base_url: https://old.example/v1\n    models:\n      stale:\n        context_length: 1\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	remote := hermesConfigPayload{
		Model: hermesModelConfig{Default: "gemma", Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{Name: "llm-proxy", BaseURL: "https://llm.example/v1", APIMode: "chat_completions", Models: map[string]hermesCustomProviderModel{
			"gemma": {ContextLength: 32768},
		}}},
	}
	if err := writeHermesConfig(path, remote); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider: openai-api", "default: gpt-5", "base_url: https://api.openai.example/v1", "name: llm-proxy", "base_url: https://llm.example/v1", "context_length: 32768"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("synced config missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "stale") || strings.Contains(string(body), "default: gemma") {
		t.Fatalf("unexpected proxy model overwrite:\n%s", body)
	}
}

func TestBootstrapHermesConfigSeedsDynamicProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	remote := hermesConfigPayload{
		Model: hermesModelConfig{Default: "gemma", Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{Name: "llm-proxy", BaseURL: "https://llm.example/v1", APIMode: "chat_completions", Models: map[string]hermesCustomProviderModel{
			"gemma": {ContextLength: 32768}, "qwen": {ContextLength: 65536},
		}}},
	}
	changed, err := bootstrapHermesConfig(path, remote)
	if err != nil || !changed {
		t.Fatalf("bootstrapHermesConfig changed=%v err=%v", changed, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider: custom:llm-proxy", "default: gemma", "name: llm-proxy", "base_url: https://llm.example/v1", "api_mode: chat_completions", "discover_models: true"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("bootstrap config missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "context_length") || strings.Contains(string(body), "qwen") {
		t.Fatalf("bootstrap should not persist a model catalog:\n%s", body)
	}
}

func TestBootstrapHermesConfigPreservesExistingChoices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "model:\n  provider: openai-api\n  default: gpt-5\ncustom_providers:\n  - name: llm-proxy\n    base_url: https://custom.example/v1\n    extra_body:\n      chat_template_kwargs:\n        enable_thinking: true\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	remote := hermesConfigPayload{
		Model:           hermesModelConfig{Default: "gemma", Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{Name: "llm-proxy", BaseURL: "https://llm.example/v1", APIMode: "chat_completions", Models: map[string]hermesCustomProviderModel{"gemma": {ContextLength: 32768}}}},
	}
	changed, err := bootstrapHermesConfig(path, remote)
	if err != nil || changed {
		t.Fatalf("bootstrapHermesConfig changed=%v err=%v", changed, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != original {
		t.Fatalf("existing config changed:\n%s", body)
	}
}

func TestBootstrapHermesConfigCompletesProxyModelWithoutReplacingProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("model:\n  provider: custom:llm-proxy\nagent:\n  max_turns: 20\n"), 0600); err != nil {
		t.Fatal(err)
	}
	remote := hermesConfigPayload{
		Model:           hermesModelConfig{Default: "gemma", Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{Name: "llm-proxy", BaseURL: "https://llm.example/v1", APIMode: "chat_completions", Models: map[string]hermesCustomProviderModel{"gemma": {ContextLength: 32768}}}},
	}
	changed, err := bootstrapHermesConfig(path, remote)
	if err != nil || !changed {
		t.Fatalf("bootstrapHermesConfig changed=%v err=%v", changed, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"max_turns: 20", "provider: custom:llm-proxy", "default: gemma", "discover_models: true"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("bootstrap config missing %q:\n%s", want, body)
		}
	}
}

func TestRunSyncHermes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"object":"vllm_proxy.hermes_config","target":"hermes-agent","config":{"model":{"default":"gemma","provider":"custom:llm-proxy"},"custom_providers":[{"name":"llm-proxy","base_url":"https://llm.example/v1","api_mode":"chat_completions","models":{"gemma":{"context_length":32768}}}]},"metadata":{}}`)), Header: make(http.Header)}, nil
	})}
	if err := runSyncHermesWithClient([]string{"--proxy-url", "https://llm.example", "--config", path}, io.Discard, client); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "default: gemma") || !strings.Contains(string(body), "provider: custom:llm-proxy") || !strings.Contains(string(body), "context_length: 32768") {
		t.Fatalf("unexpected synced config:\n%s", body)
	}
}

func TestRunBootstrapHermes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"object":"vllm_proxy.hermes_config","target":"hermes-agent","config":{"model":{"default":"gemma","provider":"custom:llm-proxy"},"custom_providers":[{"name":"llm-proxy","base_url":"https://llm.example/v1","api_mode":"chat_completions","models":{"gemma":{"context_length":32768}}}]},"metadata":{}}`)), Header: make(http.Header)}, nil
	})}
	if err := runBootstrapHermesWithClient([]string{"--proxy-url", "https://llm.example", "--config", path}, io.Discard, client); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "default: gemma") || !strings.Contains(string(body), "discover_models: true") || strings.Contains(string(body), "context_length") {
		t.Fatalf("unexpected bootstrap config:\n%s", body)
	}
}

func TestValidateHermesConfigRejectsInvalidActiveModel(t *testing.T) {
	err := validateHermesConfig(hermesConfigPayload{
		Model: hermesModelConfig{Default: "missing", Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{Name: "llm-proxy", BaseURL: "https://llm.example/v1", APIMode: "chat_completions", Models: map[string]hermesCustomProviderModel{
			"gemma": {ContextLength: 32768},
		}}},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestUpgradeLegacyHermesConfigReadsModelCatalog(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://llm.example/v1/models" {
			t.Fatalf("unexpected catalog URL: %s", request.URL)
		}
		body := `{"object":"list","data":[{"id":"gemma","metadata":{"context_length":32768}},{"id":"qwen","metadata":{"context_length":65536}}]}`
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	upgraded, err := upgradeLegacyHermesConfig(context.Background(), client, "https://llm.example", hermesConfigPayload{
		Model: hermesModelConfig{Default: "gemma", Provider: "custom", BaseURL: "https://llm.example/v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateHermesConfig(upgraded); err != nil {
		t.Fatalf("upgraded config is invalid: %v", err)
	}
	if upgraded.Model.Provider != "custom:llm-proxy" || upgraded.CustomProviders[0].Models["qwen"].ContextLength != 65536 {
		t.Fatalf("unexpected upgraded config: %#v", upgraded)
	}
}

func TestInferenceForwardsCanonicalModelName(t *testing.T) {
	const modelName = "Lorbus/Qwen3.6-27B-int4-AutoRound"
	var receivedModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		receivedModel = payload.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{
		backend: backendURL,
		registry: registry{models: map[string]modelConfig{
			modelName: {Name: modelName},
		}},
		active:  modelName,
		maxBody: 1 << 20,
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+modelName+`","messages":[]}`))
	p.inference(recorder, request)
	if recorder.Code != http.StatusOK || receivedModel != modelName {
		t.Fatalf("got status=%d backend model=%q, want %q", recorder.Code, receivedModel, modelName)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestDriversBuildRuntimeSpecificArgs(t *testing.T) {
	t.Parallel()

	model := &cogitodevv1alpha1.LLMModel{
		Spec: cogitodevv1alpha1.LLMModelSpec{
			Model:   cogitodevv1alpha1.LLMModelRef{Name: "acme/model", Source: "acme/model", Revision: "abc123"},
			Serving: cogitodevv1alpha1.ServingSpec{Args: []string{"--context-length", "8192"}},
		},
	}

	tests := []struct {
		backend cogitodevv1alpha1.BackendType
		want    []string
	}{
		{cogitodevv1alpha1.BackendVLLM, []string{"--context-length", "8192", "--model", "acme/model", "--revision", "abc123", "--served-model-name", "acme/model"}},
		{cogitodevv1alpha1.BackendSGLang, []string{"--context-length", "8192", "--model-path", "acme/model", "--revision", "abc123", "--served-model-name", "acme/model"}},
		{cogitodevv1alpha1.BackendLlamaCpp, []string{"--context-length", "8192", "-m", "acme/model", "--alias", "acme/model"}},
	}

	registry := DefaultRegistry()
	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			model.Spec.Serving.Backend = tt.backend
			driver, err := registry.Driver(tt.backend)
			if err != nil {
				t.Fatal(err)
			}
			if got := driver.EffectiveArgs(model); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EffectiveArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDriversBuildChatTemplateArgsAndRejectUnsupportedBackend(t *testing.T) {
	t.Parallel()

	model := &cogitodevv1alpha1.LLMModel{Spec: cogitodevv1alpha1.LLMModelSpec{
		Model: cogitodevv1alpha1.LLMModelRef{Name: "acme/model", Source: "acme/model"},
		Serving: cogitodevv1alpha1.ServingSpec{ChatTemplate: &cogitodevv1alpha1.ChatTemplateSpec{
			ConfigMapKeyRef: corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "template"}, Key: "chat_template.jinja"},
			SHA256:          strings.Repeat("a", 64),
		}},
	}}

	tests := []struct {
		backend cogitodevv1alpha1.BackendType
		want    []string
		wantErr bool
	}{
		{backend: cogitodevv1alpha1.BackendVLLM, want: []string{"--chat-template", chatTemplateMountPath}},
		{backend: cogitodevv1alpha1.BackendLlamaCpp, want: []string{"--jinja", "--chat-template-file", chatTemplateMountPath}},
		{backend: cogitodevv1alpha1.BackendSGLang, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			model.Spec.Serving.Backend = tt.backend
			driver, err := DefaultRegistry().Driver(tt.backend)
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Validate(model); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				got := driver.EffectiveArgs(model)
				if !reflect.DeepEqual(got[len(got)-len(tt.want):], tt.want) {
					t.Fatalf("template args = %q, want suffix %q", got, tt.want)
				}
			}
		})
	}
}

func TestDriverCapabilityContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		backend      cogitodevv1alpha1.BackendType
		healthPath   string
		cacheFormat  CacheFormat
		toolParser   bool
		reasonParser bool
	}{
		{backend: cogitodevv1alpha1.BackendVLLM, healthPath: "/health", cacheFormat: CacheFormatHuggingFace, toolParser: true, reasonParser: true},
		{backend: cogitodevv1alpha1.BackendSGLang, healthPath: "/health_generate", cacheFormat: CacheFormatHuggingFace, toolParser: true, reasonParser: true},
		{backend: cogitodevv1alpha1.BackendLlamaCpp, healthPath: "/health", cacheFormat: CacheFormatGGUF},
	}

	registry := DefaultRegistry()
	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			driver, err := registry.Driver(tt.backend)
			if err != nil {
				t.Fatal(err)
			}
			capabilities := driver.Capabilities()
			if !capabilities.OpenAIModelDiscovery {
				t.Fatal("OpenAI model discovery must be supported")
			}
			if !capabilities.Metrics {
				t.Fatal("Prometheus metrics endpoint must be supported")
			}
			if capabilities.HealthPath != tt.healthPath || capabilities.CacheFormat != tt.cacheFormat {
				t.Fatalf("capabilities = %#v, want health=%q cache=%q", capabilities, tt.healthPath, tt.cacheFormat)
			}
			if capabilities.ToolCallParser != tt.toolParser || capabilities.ReasoningParser != tt.reasonParser {
				t.Fatalf("parser capabilities = %#v", capabilities)
			}
		})
	}
}

func TestDriverHealthAndDiscoveryContracts(t *testing.T) {
	t.Parallel()

	registry := DefaultRegistry()
	for _, backendType := range []cogitodevv1alpha1.BackendType{
		cogitodevv1alpha1.BackendVLLM,
		cogitodevv1alpha1.BackendSGLang,
		cogitodevv1alpha1.BackendLlamaCpp,
	} {
		t.Run(string(backendType), func(t *testing.T) {
			driver, err := registry.Driver(backendType)
			if err != nil {
				t.Fatal(err)
			}
			httpClient := &contractHTTPClient{responses: map[string]contractResponse{
				driver.Capabilities().HealthPath: {status: http.StatusOK, body: `{"status":"ok"}`},
				"/v1/models":                     {status: http.StatusOK, body: `{"data":[{"id":"acme/one"},{"id":"acme/two"}]}`},
			}}
			if err := driver.CheckHealth(context.Background(), httpClient, "http://runtime"); err != nil {
				t.Fatal(err)
			}
			models, err := driver.DiscoverModels(context.Background(), httpClient, "http://runtime")
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"acme/one", "acme/two"}; !reflect.DeepEqual(models, want) {
				t.Fatalf("models = %q, want %q", models, want)
			}
			if got := httpClient.paths(); !reflect.DeepEqual(got, []string{driver.Capabilities().HealthPath, "/v1/models"}) {
				t.Fatalf("request paths = %q", got)
			}
			unhealthyClient := &contractHTTPClient{responses: map[string]contractResponse{
				driver.Capabilities().HealthPath: {status: http.StatusServiceUnavailable, body: "loading"},
			}}
			if err := driver.CheckHealth(context.Background(), unhealthyClient, "http://runtime"); err == nil {
				t.Fatal("health check accepted a non-200 response")
			}
		})
	}
}

func TestDriverRuntimeMetadataContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		backend        cogitodevv1alpha1.BackendType
		args           []string
		wantConcurrent int
		wantKV         map[string]string
		wantMetrics    bool
	}{
		{
			backend:        cogitodevv1alpha1.BackendVLLM,
			args:           []string{"--max-num-seqs", "7"},
			wantConcurrent: 7,
			wantKV:         map[string]string{"block_size": "16", "cache_dtype": "fp8"},
			wantMetrics:    true,
		},
		{
			backend:        cogitodevv1alpha1.BackendSGLang,
			args:           []string{"--max-running-requests", "9"},
			wantConcurrent: 9,
		},
		{
			backend:        cogitodevv1alpha1.BackendLlamaCpp,
			args:           []string{"--parallel", "3"},
			wantConcurrent: 3,
		},
	}

	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			model := contractModel(tt.backend)
			model.Spec.Serving.Args = tt.args
			driver, err := DefaultRegistry().Driver(tt.backend)
			if err != nil {
				t.Fatal(err)
			}
			httpClient := &contractHTTPClient{responses: map[string]contractResponse{
				"/v1/models": {status: http.StatusOK, body: `{"data":[{"id":"acme/model"}]}`},
				"/metrics":   {status: http.StatusOK, body: `vllm:cache_config_info{block_size="16",cache_dtype="fp8"} 1`},
			}}
			metadata, err := driver.CollectRuntimeMetadata(context.Background(), httpClient, "http://runtime", model)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.ContextLength != model.Spec.Serving.MaxModelLen || metadata.MaxConcurrentReqs != tt.wantConcurrent {
				t.Fatalf("metadata = %#v", metadata)
			}
			if !reflect.DeepEqual(metadata.ServedModelIDs, []string{"acme/model"}) || !reflect.DeepEqual(metadata.KVCache, tt.wantKV) {
				t.Fatalf("runtime metadata = %#v, want KV %#v", metadata, tt.wantKV)
			}
			if got := containsString(httpClient.paths(), "/metrics"); got != tt.wantMetrics {
				t.Fatalf("metrics requested = %t, want %t; paths=%q", got, tt.wantMetrics, httpClient.paths())
			}
		})
	}
}

func TestDriverCacheRequestContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		backend        cogitodevv1alpha1.BackendType
		wantRepository string
		wantFormat     CacheFormat
		wantCacheKind  string
	}{
		{backend: cogitodevv1alpha1.BackendVLLM, wantRepository: "acme/source", wantFormat: CacheFormatHuggingFace, wantCacheKind: "huggingface-hub"},
		{backend: cogitodevv1alpha1.BackendSGLang, wantRepository: "acme/source", wantFormat: CacheFormatHuggingFace, wantCacheKind: "huggingface-hub"},
		{backend: cogitodevv1alpha1.BackendLlamaCpp, wantRepository: "acme/gguf", wantFormat: CacheFormatGGUF, wantCacheKind: "huggingface-files"},
	}
	for _, tt := range tests {
		t.Run(string(tt.backend), func(t *testing.T) {
			model := contractModel(tt.backend)
			model.Spec.Model.Source = "acme/source"
			model.Spec.Model.ArtifactRepository = "acme/gguf"
			model.Spec.Artifact = &cogitodevv1alpha1.ArtifactSpec{ExpectedSize: "2Gi", Files: []string{"model.gguf"}}
			if tt.backend == cogitodevv1alpha1.BackendLlamaCpp {
				model.Spec.Artifact.MaterializationTarget = "gguf/acme-model-abc123"
			}
			driver, err := DefaultRegistry().Driver(tt.backend)
			if err != nil {
				t.Fatal(err)
			}
			request, err := driver.CacheRequest(model)
			if err != nil {
				t.Fatal(err)
			}
			if driver.Capabilities().CacheFormat != tt.wantFormat {
				t.Fatalf("cache format = %q, want %q", driver.Capabilities().CacheFormat, tt.wantFormat)
			}
			if request.Cache.RepoID != tt.wantRepository || request.Cache.Kind != tt.wantCacheKind || request.Cache.Size != 2*1024*1024*1024 {
				t.Fatalf("cache request = %#v", request)
			}
			if tt.backend == cogitodevv1alpha1.BackendLlamaCpp && request.Cache.MaterializationTarget != "gguf/acme-model-abc123" {
				t.Fatalf("cache target = %q", request.Cache.MaterializationTarget)
			}
		})
	}
}

func TestLlamaCPPRejectsUnsafeMaterializationTarget(t *testing.T) {
	t.Parallel()
	model := contractModel(cogitodevv1alpha1.BackendLlamaCpp)
	model.Spec.Model.ArtifactRepository = "acme/gguf"
	model.Spec.Artifact = &cogitodevv1alpha1.ArtifactSpec{ExpectedSize: "2Gi", Files: []string{"model.gguf"}, MaterializationTarget: "../escape"}
	driver, err := DefaultRegistry().Driver(cogitodevv1alpha1.BackendLlamaCpp)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Validate(model); err == nil {
		t.Fatal("llama.cpp accepted an unsafe materialization target")
	}
}

func TestLlamaCPPRejectsIncompleteGGUFCacheConfiguration(t *testing.T) {
	t.Parallel()

	model := contractModel(cogitodevv1alpha1.BackendLlamaCpp)
	model.Spec.Artifact = &cogitodevv1alpha1.ArtifactSpec{ExpectedSize: "2Gi"}
	driver, err := DefaultRegistry().Driver(cogitodevv1alpha1.BackendLlamaCpp)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Validate(model); err == nil {
		t.Fatal("llama.cpp accepted a cached GGUF model without repository/files")
	}
}

func TestDriverRejectsInjectedArgs(t *testing.T) {
	t.Parallel()

	driver, err := DefaultRegistry().Driver(cogitodevv1alpha1.BackendSGLang)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.ValidateArgs([]string{"--model-path=override"}); err == nil {
		t.Fatal("ValidateArgs() accepted controller-injected flag")
	}
}

func TestStructuredParsers(t *testing.T) {
	t.Parallel()

	model := &cogitodevv1alpha1.LLMModel{
		Spec: cogitodevv1alpha1.LLMModelSpec{
			Model: cogitodevv1alpha1.LLMModelRef{Name: "acme/model", Source: "acme/model"},
			Serving: cogitodevv1alpha1.ServingSpec{
				ToolCallParser:  "hermes",
				ReasoningParser: "deepseek-r1",
			},
		},
	}

	registry := DefaultRegistry()
	for _, backendType := range []cogitodevv1alpha1.BackendType{cogitodevv1alpha1.BackendVLLM, cogitodevv1alpha1.BackendSGLang} {
		t.Run(string(backendType), func(t *testing.T) {
			model.Spec.Serving.Backend = backendType
			driver, err := registry.Driver(backendType)
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Validate(model); err != nil {
				t.Fatal(err)
			}
			got := driver.EffectiveArgs(model)
			wantSuffix := []string{"--tool-call-parser", "hermes", "--reasoning-parser", "deepseek-r1"}
			if !reflect.DeepEqual(got[len(got)-len(wantSuffix):], wantSuffix) {
				t.Errorf("parser args = %q, want suffix %q", got, wantSuffix)
			}
		})
	}

	model.Spec.Serving.Backend = cogitodevv1alpha1.BackendLlamaCpp
	driver, err := registry.Driver(model.Spec.Serving.Backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Validate(model); err == nil {
		t.Fatal("llama.cpp accepted unsupported structured parsers")
	}
}

func TestStructuredParserRejectsRawConflict(t *testing.T) {
	t.Parallel()

	model := &cogitodevv1alpha1.LLMModel{Spec: cogitodevv1alpha1.LLMModelSpec{
		Serving: cogitodevv1alpha1.ServingSpec{
			ToolCallParser: "hermes",
			Args:           []string{"--tool-call-parser", "legacy"},
		},
	}}
	driver, err := DefaultRegistry().Driver(cogitodevv1alpha1.BackendVLLM)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Validate(model); err == nil {
		t.Fatal("Validate() accepted a structured/raw parser conflict")
	}
}

func contractModel(backendType cogitodevv1alpha1.BackendType) *cogitodevv1alpha1.LLMModel {
	return &cogitodevv1alpha1.LLMModel{Spec: cogitodevv1alpha1.LLMModelSpec{
		Model: cogitodevv1alpha1.LLMModelRef{Name: "acme/model", Source: "acme/model", Revision: "abc123"},
		Serving: cogitodevv1alpha1.ServingSpec{
			Backend:     backendType,
			DisplayName: "Model",
			MaxModelLen: 8192,
			Args:        []string{},
		},
	}}
}

type contractResponse struct {
	status int
	body   string
}

type contractHTTPClient struct {
	mu        sync.Mutex
	responses map[string]contractResponse
	requests  []string
}

func (c *contractHTTPClient) Do(request *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, request.URL.Path)
	response, ok := c.responses[request.URL.Path]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
	}
	return &http.Response{
		StatusCode: response.status,
		Status:     fmt.Sprintf("%d %s", response.status, http.StatusText(response.status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(response.body)),
	}, nil
}

func (c *contractHTTPClient) paths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

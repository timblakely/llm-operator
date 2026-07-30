// Package backend translates portable model intent into runtime-specific
// activation, observation, and cache behavior.
package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
	"github.com/timblakely/llm-operator/internal/cache"
)

const maxResponseBody = 8 << 20

// CacheFormat describes the artifact layout a runtime consumes.
type CacheFormat string

const (
	CacheFormatHuggingFace CacheFormat = "huggingface-hub"
	CacheFormatGGUF        CacheFormat = "gguf"
)

// Capabilities is the explicit contract exposed by a compiled-in backend.
// Metrics indicates that the runtime has a Prometheus endpoint; individual
// metric families remain runtime-specific.
type Capabilities struct {
	OpenAIModelDiscovery bool
	ToolCallParser       bool
	ReasoningParser      bool
	Metrics              bool
	CacheFormat          CacheFormat
	HealthPath           string
}

// HTTPDoer is the subset of http.Client used by backend drivers.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Driver encapsulates every runtime-specific behavior used by controllers.
// Implementations must not mutate the supplied model.
type Driver interface {
	Type() cogitodevv1alpha1.BackendType
	Capabilities() Capabilities
	Validate(model *cogitodevv1alpha1.LLMModel) error
	ValidateArgs(args []string) error
	EffectiveArgs(model *cogitodevv1alpha1.LLMModel) []string
	CheckHealth(context.Context, HTTPDoer, string) error
	DiscoverModels(context.Context, HTTPDoer, string) ([]string, error)
	CollectRuntimeMetadata(context.Context, HTTPDoer, string, *cogitodevv1alpha1.LLMModel) (*cogitodevv1alpha1.RuntimeMetadata, error)
	CacheRequest(*cogitodevv1alpha1.LLMModel) (*cache.CacheRequest, error)
}

// Registry provides compiled-in backend drivers. Unsupported runtimes fail
// validation instead of silently receiving another runtime's semantics.
type Registry struct {
	drivers map[cogitodevv1alpha1.BackendType]Driver
}

func NewRegistry(drivers ...Driver) *Registry {
	r := &Registry{drivers: make(map[cogitodevv1alpha1.BackendType]Driver, len(drivers))}
	for _, driver := range drivers {
		r.drivers[driver.Type()] = driver
	}
	return r
}

func DefaultRegistry() *Registry {
	return NewRegistry(vllmDriver, llamaCPPDriver, sglangDriver)
}

func (r *Registry) Driver(kind cogitodevv1alpha1.BackendType) (Driver, error) {
	driver, ok := r.drivers[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported backend type %q", kind)
	}
	return driver, nil
}

type runtimeDriver struct {
	kind                cogitodevv1alpha1.BackendType
	capabilities        Capabilities
	injectedFlags       []string
	toolCallFlag        string
	reasoningFlag       string
	maxConcurrencyFlags []string
	build               func(*cogitodevv1alpha1.LLMModel, []string) []string
	parseMetrics        func(string) map[string]string
}

func (d runtimeDriver) Type() cogitodevv1alpha1.BackendType { return d.kind }
func (d runtimeDriver) Capabilities() Capabilities          { return d.capabilities }

func (d runtimeDriver) ValidateArgs(args []string) error {
	for _, arg := range args {
		for _, flag := range d.injectedFlags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") || strings.HasPrefix(arg, flag+" ") {
				return fmt.Errorf("arg %q is a controller-injected flag and must not be in spec.serving.args", arg)
			}
		}
	}
	return nil
}

func (d runtimeDriver) Validate(model *cogitodevv1alpha1.LLMModel) error {
	if model.Spec.Serving.Backend != "" && model.Spec.Serving.Backend != d.kind {
		return fmt.Errorf("driver %q cannot serve model configured for backend %q", d.kind, model.Spec.Serving.Backend)
	}
	if err := d.ValidateArgs(model.Spec.Serving.Args); err != nil {
		return err
	}
	if model.Spec.Serving.ToolCallParser != "" {
		if !d.capabilities.ToolCallParser {
			return fmt.Errorf("backend %q does not support toolCallParser", d.kind)
		}
		if containsFlag(model.Spec.Serving.Args, d.toolCallFlag) {
			return fmt.Errorf("toolCallParser cannot be used with raw %q args", d.toolCallFlag)
		}
	}
	if model.Spec.Serving.ReasoningParser != "" {
		if !d.capabilities.ReasoningParser {
			return fmt.Errorf("backend %q does not support reasoningParser", d.kind)
		}
		if containsFlag(model.Spec.Serving.Args, d.reasoningFlag) {
			return fmt.Errorf("reasoningParser cannot be used with raw %q args", d.reasoningFlag)
		}
	}
	if model.Spec.Artifact != nil {
		if model.Spec.Artifact.ExpectedSize != "" {
			if _, err := parseSize(model.Spec.Artifact.ExpectedSize); err != nil {
				return fmt.Errorf("invalid artifact expectedSize %q: %w", model.Spec.Artifact.ExpectedSize, err)
			}
		}
		if d.capabilities.CacheFormat == CacheFormatGGUF {
			if model.Spec.Model.ArtifactRepository == "" {
				return fmt.Errorf("backend %q requires model.artifactRepository for cached GGUF artifacts", d.kind)
			}
			if len(model.Spec.Artifact.Files) == 0 {
				return fmt.Errorf("backend %q requires artifact.files for cached GGUF artifacts", d.kind)
			}
		}
	}
	return nil
}

func (d runtimeDriver) EffectiveArgs(model *cogitodevv1alpha1.LLMModel) []string {
	args := append([]string(nil), model.Spec.Serving.Args...)
	args = d.build(model, args)
	if model.Spec.Serving.ToolCallParser != "" && d.toolCallFlag != "" {
		args = append(args, d.toolCallFlag, model.Spec.Serving.ToolCallParser)
	}
	if model.Spec.Serving.ReasoningParser != "" && d.reasoningFlag != "" {
		args = append(args, d.reasoningFlag, model.Spec.Serving.ReasoningParser)
	}
	return args
}

func (d runtimeDriver) CheckHealth(ctx context.Context, httpClient HTTPDoer, baseURL string) error {
	_, err := getText(ctx, httpClient, strings.TrimSuffix(baseURL, "/")+d.capabilities.HealthPath, 4096)
	if err != nil {
		return fmt.Errorf("%s health check: %w", d.kind, err)
	}
	return nil
}

func (d runtimeDriver) DiscoverModels(ctx context.Context, httpClient HTTPDoer, baseURL string) ([]string, error) {
	if !d.capabilities.OpenAIModelDiscovery {
		return nil, fmt.Errorf("backend %q does not support OpenAI model discovery", d.kind)
	}
	body, err := getText(ctx, httpClient, strings.TrimSuffix(baseURL, "/")+"/v1/models", maxResponseBody)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		return nil, fmt.Errorf("decode %s model discovery response: %w", d.kind, err)
	}
	ids := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids, nil
}

func (d runtimeDriver) CollectRuntimeMetadata(ctx context.Context, httpClient HTTPDoer, baseURL string, model *cogitodevv1alpha1.LLMModel) (*cogitodevv1alpha1.RuntimeMetadata, error) {
	meta := &cogitodevv1alpha1.RuntimeMetadata{
		ObservedAt:      metav1.Now(),
		ContextLength:   model.Spec.Serving.MaxModelLen,
		LaunchArguments: launchArguments(d.EffectiveArgs(model)),
	}

	var collectionErrors []error
	if d.capabilities.OpenAIModelDiscovery {
		models, err := d.DiscoverModels(ctx, httpClient, baseURL)
		if err != nil {
			collectionErrors = append(collectionErrors, err)
		} else {
			meta.ServedModelIDs = models
		}
	}
	for _, flag := range d.maxConcurrencyFlags {
		if value, ok := meta.LaunchArguments[flag]; ok {
			if n, err := strconv.Atoi(value); err == nil {
				meta.MaxConcurrentReqs = n
				break
			}
		}
	}
	if d.parseMetrics != nil {
		metricsText, err := getText(ctx, httpClient, strings.TrimSuffix(baseURL, "/")+"/metrics", maxResponseBody)
		if err != nil {
			collectionErrors = append(collectionErrors, err)
		} else {
			meta.KVCache = d.parseMetrics(metricsText)
		}
	}
	return meta, errors.Join(collectionErrors...)
}

func (d runtimeDriver) CacheRequest(model *cogitodevv1alpha1.LLMModel) (*cache.CacheRequest, error) {
	if model.Spec.Artifact == nil {
		return nil, nil
	}
	if err := d.Validate(model); err != nil {
		return nil, err
	}
	repository := model.Spec.Model.Source
	if d.capabilities.CacheFormat == CacheFormatGGUF {
		repository = model.Spec.Model.ArtifactRepository
	}
	cacheSpec := cache.CacheSpec{
		Kind:     cacheManagerKind(d.capabilities.CacheFormat),
		RepoID:   repository,
		Revision: model.Spec.Model.Revision,
		Files:    append([]string(nil), model.Spec.Artifact.Files...),
	}
	if model.Spec.Artifact.ExpectedSize != "" {
		size, err := parseSize(model.Spec.Artifact.ExpectedSize)
		if err != nil {
			return nil, err
		}
		cacheSpec.Size = size
	}
	return &cache.CacheRequest{
		Model:   model.Spec.Model.Name,
		Backend: string(d.kind),
		Cache:   cacheSpec,
	}, nil
}

func cacheManagerKind(format CacheFormat) string {
	switch format {
	case CacheFormatHuggingFace:
		return "huggingface-hub"
	case CacheFormatGGUF:
		return "huggingface-files"
	default:
		return string(format)
	}
}

func getText(ctx context.Context, httpClient HTTPDoer, url string, limit int64) (string, error) {
	if httpClient == nil {
		return "", errors.New("HTTP client is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit))
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

func containsFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

func launchArguments(args []string) map[string]string {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			values[args[i]] = args[i+1]
			i++
		} else {
			values[args[i]] = "true"
		}
	}
	return values
}

func parseVLLMCacheConfig(metricsText string) map[string]string {
	scanner := bufio.NewScanner(strings.NewReader(metricsText))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "vllm:cache_config_info{") {
			continue
		}
		start := strings.IndexByte(line, '{')
		end := strings.LastIndex(line, "}")
		if start < 0 || end <= start {
			return nil
		}
		return parsePrometheusLabels(line[start+1 : end])
	}
	return nil
}

func parsePrometheusLabels(encoded string) map[string]string {
	labels := map[string]string{}
	for len(encoded) > 0 {
		equals := strings.IndexByte(encoded, '=')
		if equals < 1 || equals+1 >= len(encoded) || encoded[equals+1] != '"' {
			return labels
		}
		key := encoded[:equals]
		encoded = encoded[equals+1:]
		end := 1
		for end < len(encoded) {
			if encoded[end] == '"' && encoded[end-1] != '\\' {
				break
			}
			end++
		}
		if end >= len(encoded) {
			return labels
		}
		value, err := strconv.Unquote(encoded[:end+1])
		if err != nil {
			return labels
		}
		labels[key] = value
		encoded = strings.TrimPrefix(encoded[end+1:], ",")
	}
	return labels
}

func parseSize(size string) (int64, error) {
	var multiplier int64 = 1
	for suffix, value := range map[string]int64{
		"Gi": 1024 * 1024 * 1024,
		"Mi": 1024 * 1024,
		"Ki": 1024,
		"G":  1000 * 1000 * 1000,
		"M":  1000 * 1000,
		"K":  1000,
	} {
		if strings.HasSuffix(size, suffix) {
			multiplier = value
			size = strings.TrimSuffix(size, suffix)
			break
		}
	}
	value, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, errors.New("size must be greater than zero")
	}
	return value * multiplier, nil
}

var vllmDriver = runtimeDriver{
	kind: cogitodevv1alpha1.BackendVLLM,
	capabilities: Capabilities{
		OpenAIModelDiscovery: true,
		ToolCallParser:       true,
		ReasoningParser:      true,
		Metrics:              true,
		CacheFormat:          CacheFormatHuggingFace,
		HealthPath:           "/health",
	},
	injectedFlags:       []string{"--model", "--revision", "--served-model-name"},
	toolCallFlag:        "--tool-call-parser",
	reasoningFlag:       "--reasoning-parser",
	maxConcurrencyFlags: []string{"--max-num-seqs"},
	build: func(model *cogitodevv1alpha1.LLMModel, args []string) []string {
		args = append(args, "--model", model.Spec.Model.Source)
		if model.Spec.Model.Revision != "" {
			args = append(args, "--revision", model.Spec.Model.Revision)
		}
		return append(args, "--served-model-name", model.Spec.Model.Name)
	},
	parseMetrics: parseVLLMCacheConfig,
}

var llamaCPPDriver = runtimeDriver{
	kind: cogitodevv1alpha1.BackendLlamaCpp,
	capabilities: Capabilities{
		OpenAIModelDiscovery: true,
		Metrics:              true,
		CacheFormat:          CacheFormatGGUF,
		HealthPath:           "/health",
	},
	injectedFlags:       []string{"-m", "--model", "--alias"},
	maxConcurrencyFlags: []string{"--parallel", "-np"},
	build: func(model *cogitodevv1alpha1.LLMModel, args []string) []string {
		return append(args, "-m", model.Spec.Model.Source, "--alias", model.Spec.Model.Name)
	},
}

// SGLang's generation health endpoint verifies worker readiness rather than
// only reporting that the HTTP process is alive.
var sglangDriver = runtimeDriver{
	kind: cogitodevv1alpha1.BackendSGLang,
	capabilities: Capabilities{
		OpenAIModelDiscovery: true,
		ToolCallParser:       true,
		ReasoningParser:      true,
		Metrics:              true,
		CacheFormat:          CacheFormatHuggingFace,
		HealthPath:           "/health_generate",
	},
	injectedFlags:       []string{"--model-path", "--revision", "--served-model-name"},
	toolCallFlag:        "--tool-call-parser",
	reasoningFlag:       "--reasoning-parser",
	maxConcurrencyFlags: []string{"--max-running-requests"},
	build: func(model *cogitodevv1alpha1.LLMModel, args []string) []string {
		args = append(args, "--model-path", model.Spec.Model.Source)
		if model.Spec.Model.Revision != "" {
			args = append(args, "--revision", model.Spec.Model.Revision)
		}
		return append(args, "--served-model-name", model.Spec.Model.Name)
	},
}

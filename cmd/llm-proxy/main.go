package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	activeModelAnno  = "llm.cogito.dev/active-model"
	switchedAtAnno   = "llm.cogito.dev/switched-at"
	modelStatusName  = "llm-model-status"
	defaultMaxBody   = 32 << 20
	defaultTimeout   = 30 * time.Minute
	defaultSweep     = 10 * time.Minute
	backendProbeWait = 2 * time.Second
	activeModelName  = "default"
)

var (
	llmModelGVR    = schema.GroupVersionResource{Group: "llm.cogito.dev", Version: "v1alpha1", Resource: "llmmodels"}
	llmOverlayGVR  = schema.GroupVersionResource{Group: "llm.cogito.dev", Version: "v1alpha1", Resource: "llmmodeloverlays"}
	activeModelGVR = schema.GroupVersionResource{Group: "llm.cogito.dev", Version: "v1alpha1", Resource: "llmactivemodels"}
)

type modelConfig struct {
	Name        string
	Backend     string
	ModelSource string
	DisplayName string
	MaxModelLen int
	Created     time.Time
	Args        []string
	Source      string
	Fallback    json.RawMessage
	Runtime     json.RawMessage
	Cache       cacheSpec
}

// cacheSpec describes an immutable model artifact.  It is intentionally kept
// separate from ModelSource: ModelSource is an argument to a serving runtime,
// while this identifies bytes that may safely be restored from cold storage.
type cacheSpec struct {
	Kind                  string   `json:"kind"`
	RepoID                string   `json:"repo_id"`
	Revision              string   `json:"revision"`
	Size                  int64    `json:"size_bytes"`
	Files                 []string `json:"files,omitempty"`
	MaterializationTarget string   `json:"materialization_target,omitempty"`
}

// backendConfig is deliberately configured by the Helm release, not by model
// resources. A model may select a known runtime, but cannot direct the proxy
// to arbitrary Services, Deployments, or containers.
type backendConfig struct {
	Name       string
	Deployment string
	Container  string
	URL        *url.URL
}

// overlayConfig is a virtual chat model. It selects BaseModel for vLLM while
// merging request defaults into the client request before it is forwarded.
type overlayConfig struct {
	Name            string
	DisplayName     string
	BaseModel       string
	Created         time.Time
	RequestDefaults json.RawMessage
	Source          string
}

type runtimeMetadata struct {
	SchemaVersion         int               `json:"schema_version"`
	Source                string            `json:"source"`
	ObservedAt            time.Time         `json:"observed_at"`
	ModelName             string            `json:"model_name"`
	ServedModelIDs        []string          `json:"served_model_ids,omitempty"`
	ContextLength         int               `json:"context_length"`
	MaxConcurrentRequests int               `json:"max_concurrent_requests,omitempty"`
	LaunchArguments       map[string]string `json:"launch_arguments"`
	KVCache               map[string]string `json:"kv_cache,omitempty"`
}

type registry struct {
	models   map[string]modelConfig
	overlays map[string]overlayConfig
}

type proxy struct {
	client          kubernetes.Interface
	dynamic         dynamic.Interface
	namespace       string
	deployment      string
	container       string
	backend         *url.URL
	backendName     string
	backends        map[string]backendConfig
	publicBaseURL   string
	httpClient      *http.Client
	transitionLimit time.Duration
	maxBody         int64
	cacheManager    *url.URL
	// readOnlyTransitions keeps the catalog, overlay, and backend-observation
	// paths live while preventing this proxy from changing Deployments. It is
	// used for the operator handoff canary; false is deliberately the zero value
	// so existing callers and tests retain the current transition behavior.
	readOnlyTransitions bool

	stateMu         sync.RWMutex
	registry        registry
	active          string
	transitioning   bool
	transitionModel string
	// transitionCancel interrupts an obsolete rollout. The next reconciliation
	// applies the current desired model's arguments.
	transitionCancel context.CancelFunc
	reconcilePending bool
	ready            bool
	startedAt        time.Time
	activeSince      time.Time

	switchesTotal atomic.Uint64
	configErrors  atomic.Uint64
	cacheHotHits  atomic.Uint64
	cacheColdHits atomic.Uint64
	cacheExternal atomic.Uint64
	lastSwitch    atomic.Int64 // nanoseconds
	lastStart     atomic.Int64 // nanoseconds
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "cache-manager" {
		runCacheManager()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "generate-config" {
		if err := runGenerateConfig(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "generate-config:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "sync-hermes" {
		if err := runSyncHermes(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "sync-hermes:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "bootstrap-hermes" {
		if err := runBootstrapHermes(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "bootstrap-hermes:", err)
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("load in-cluster Kubernetes config", "error", err)
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error("create Kubernetes client", "error", err)
		os.Exit(1)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logger.Error("create dynamic Kubernetes client", "error", err)
		os.Exit(1)
	}
	backend, err := url.Parse(env("BACKEND_URL", "http://llm-vllm:8000"))
	if err != nil {
		logger.Error("parse BACKEND_URL", "error", err)
		os.Exit(1)
	}
	llamaBackend, err := url.Parse(env("LLAMA_BACKEND_URL", "http://llm-laguna:8000"))
	if err != nil {
		logger.Error("parse LLAMA_BACKEND_URL", "error", err)
		os.Exit(1)
	}
	var cacheManager *url.URL
	if raw := strings.TrimSpace(os.Getenv("CACHE_MANAGER_URL")); raw != "" {
		cacheManager, err = url.Parse(raw)
		if err != nil {
			logger.Error("parse CACHE_MANAGER_URL", "error", err)
			os.Exit(1)
		}
	}
	vllmDeployment := env("VLLM_DEPLOYMENT", "llm-vllm")
	vllmContainer := env("VLLM_CONTAINER", "vllm")
	p := &proxy{
		client:      client,
		dynamic:     dynamicClient,
		namespace:   env("POD_NAMESPACE", mustNamespace()),
		deployment:  vllmDeployment,
		container:   vllmContainer,
		backend:     backend,
		backendName: "vllm",
		backends: map[string]backendConfig{
			"vllm":      {Name: "vllm", Deployment: vllmDeployment, Container: vllmContainer, URL: backend},
			"llama-cpp": {Name: "llama-cpp", Deployment: env("LLAMA_DEPLOYMENT", "llm-laguna"), Container: env("LLAMA_CONTAINER", "laguna"), URL: llamaBackend},
		},
		active:              env("DEFAULT_MODEL", ""),
		publicBaseURL:       strings.TrimSuffix(env("PUBLIC_BASE_URL", "http://llm-proxy:8080/v1"), "/"),
		httpClient:          &http.Client{Timeout: 15 * time.Second},
		transitionLimit:     durationEnv("TRANSITION_TIMEOUT", defaultTimeout),
		cacheManager:        cacheManager,
		maxBody:             int64Env("MAX_REQUEST_BODY_BYTES", defaultMaxBody),
		readOnlyTransitions: !boolEnv("ENABLE_DEPLOYMENT_MUTATIONS", true),
		startedAt:           time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := p.refresh(ctx); err != nil {
		logger.Warn("initial model registry load failed; will retry", "error", err)
	}
	cancel()
	if !p.readOnlyTransitions {
		go p.reconcileActiveDeployment(logger)
	} else {
		logger.Info("deployment mutations disabled; proxy is serving catalog and overlays read-only")
	}
	go p.sweepCache(logger)
	go p.watchLLMResources(logger)
	go p.watchDeployments(logger)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", p.healthz)
	mux.HandleFunc("GET /readyz", p.readyz)
	mux.HandleFunc("GET /metrics", p.metrics)
	mux.HandleFunc("GET /v1/models", p.models)
	mux.HandleFunc("GET /v1/models/{id}", p.model)
	mux.HandleFunc("GET /vllm-proxy/config/{target}", p.hermesConfig)
	mux.HandleFunc("/v1/", p.inference)

	server := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	logger.Info("vLLM proxy listening", "address", server.Addr, "backend", backend.String())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve", "error", err)
		os.Exit(1)
	}
}

type hermesConfigResponse struct {
	Object   string              `json:"object"`
	Target   string              `json:"target"`
	Config   hermesConfigPayload `json:"config"`
	Metadata map[string]any      `json:"metadata"`
}

type hermesConfigPayload struct {
	Model           hermesModelConfig      `json:"model"`
	CustomProviders []hermesCustomProvider `json:"custom_providers"`
}

type hermesModelConfig struct {
	Default  string `json:"default"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
}

type hermesCustomProvider struct {
	Name    string                               `json:"name"`
	BaseURL string                               `json:"base_url"`
	APIMode string                               `json:"api_mode"`
	Models  map[string]hermesCustomProviderModel `json:"models"`
}

type hermesCustomProviderModel struct {
	ContextLength int `json:"context_length"`
}

func (p *proxy) hermesConfig(w http.ResponseWriter, r *http.Request) {
	p.stateMu.RLock()
	cfg, ok := p.registry.models[p.active]
	models := make([]modelConfig, 0, len(p.registry.models))
	for _, model := range p.registry.models {
		models = append(models, model)
	}
	p.stateMu.RUnlock()
	if !ok {
		openAIError(w, http.StatusServiceUnavailable, "server_error", "No active model is available.")
		return
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	providerModels := make(map[string]hermesCustomProviderModel, len(models))
	for _, model := range models {
		providerModels[model.Name] = hermesCustomProviderModel{ContextLength: model.MaxModelLen}
	}
	card := modelCard(cfg)
	metadata, _ := card["metadata"].(map[string]any)
	writeJSON(w, http.StatusOK, hermesConfigResponse{
		Object: "vllm_proxy.hermes_config",
		Target: r.PathValue("target"),
		Config: hermesConfigPayload{
			Model: hermesModelConfig{Default: cfg.Name, Provider: "custom:llm-proxy"},
			CustomProviders: []hermesCustomProvider{{
				Name: "llm-proxy", BaseURL: p.publicBaseURL, APIMode: "chat_completions", Models: providerModels,
			}},
		},
		Metadata: metadata,
	})
}

func runSyncHermes(args []string, output io.Writer) error {
	return runSyncHermesWithClient(args, output, http.DefaultClient)
}

func runSyncHermesWithClient(args []string, output io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("sync-hermes", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	proxyURL := flags.String("proxy-url", env("VLLM_PROXY_URL", ""), "vLLM proxy base URL")
	target := flags.String("target", "hermes-agent", "proxy configuration target")
	configPath := flags.String("config", defaultHermesConfigPath(), "Hermes config.yaml path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: vllm-proxy sync-hermes --proxy-url URL [--config PATH] [--target NAME]")
	}
	if *proxyURL == "" {
		return errors.New("--proxy-url or VLLM_PROXY_URL is required")
	}
	remote, endpoint, err := fetchHermesConfig(client, *proxyURL, *target)
	if err != nil {
		return err
	}
	if err := writeHermesConfig(*configPath, remote.Config); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Updated %s with active model %s from %s.\n", *configPath, remote.Config.Model.Default, endpoint)
	return err
}

// runBootstrapHermes installs the minimum named-provider configuration needed
// for Hermes to discover the proxy's live /v1/models catalog. Unlike
// sync-hermes, it never reconciles an existing model choice or provider entry.
func runBootstrapHermes(args []string, output io.Writer) error {
	return runBootstrapHermesWithClient(args, output, http.DefaultClient)
}

func runBootstrapHermesWithClient(args []string, output io.Writer, client *http.Client) error {
	flags := flag.NewFlagSet("bootstrap-hermes", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	proxyURL := flags.String("proxy-url", env("VLLM_PROXY_URL", ""), "vLLM proxy base URL")
	target := flags.String("target", "hermes-agent", "proxy configuration target")
	configPath := flags.String("config", defaultHermesConfigPath(), "Hermes config.yaml path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: vllm-proxy bootstrap-hermes --proxy-url URL [--config PATH] [--target NAME]")
	}
	if *proxyURL == "" {
		return errors.New("--proxy-url or VLLM_PROXY_URL is required")
	}
	remote, endpoint, err := fetchHermesConfig(client, *proxyURL, *target)
	if err != nil {
		return err
	}
	changed, err := bootstrapHermesConfig(*configPath, remote.Config)
	if err != nil {
		return err
	}
	if changed {
		_, err = fmt.Fprintf(output, "Bootstrapped %s with llm-proxy (initial model %s) from %s.\n", *configPath, remote.Config.Model.Default, endpoint)
	} else {
		_, err = fmt.Fprintf(output, "%s already has Hermes model and llm-proxy settings; left unchanged.\n", *configPath)
	}
	return err
}

func fetchHermesConfig(client *http.Client, proxyURL, target string) (hermesConfigResponse, string, error) {
	u, err := url.Parse(strings.TrimSuffix(proxyURL, "/") + "/vllm-proxy/config/" + url.PathEscape(target))
	if err != nil {
		return hermesConfigResponse{}, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return hermesConfigResponse{}, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return hermesConfigResponse{}, "", fmt.Errorf("fetch proxy config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hermesConfigResponse{}, "", fmt.Errorf("fetch proxy config: proxy returned %s", resp.Status)
	}
	var remote hermesConfigResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&remote); err != nil {
		return hermesConfigResponse{}, "", fmt.Errorf("decode proxy config: %w", err)
	}
	if err := validateHermesConfig(remote.Config); err != nil {
		upgraded, upgradeErr := upgradeLegacyHermesConfig(ctx, client, proxyURL, remote.Config)
		if upgradeErr != nil {
			return hermesConfigResponse{}, "", fmt.Errorf("invalid proxy Hermes configuration: %w", err)
		}
		remote.Config = upgraded
	}
	return remote, u.String(), nil
}

type openAIModelsResponse struct {
	Data []struct {
		ID       string `json:"id"`
		Metadata struct {
			ContextLength int `json:"context_length"`
		} `json:"metadata"`
	} `json:"data"`
}

// upgradeLegacyHermesConfig supports a proxy that predates its native Hermes
// catalog endpoint. Its OpenAI-compatible /v1/models response already contains
// the full registry and per-model context limits needed for the catalog.
func upgradeLegacyHermesConfig(ctx context.Context, client *http.Client, proxyURL string, legacy hermesConfigPayload) (hermesConfigPayload, error) {
	if legacy.Model.Provider != "custom" || legacy.Model.Default == "" || legacy.Model.BaseURL == "" {
		return hermesConfigPayload{}, errors.New("not a legacy custom proxy configuration")
	}
	endpoint, err := url.Parse(strings.TrimSuffix(proxyURL, "/") + "/v1/models")
	if err != nil {
		return hermesConfigPayload{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return hermesConfigPayload{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return hermesConfigPayload{}, fmt.Errorf("fetch proxy model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hermesConfigPayload{}, fmt.Errorf("fetch proxy model catalog: proxy returned %s", resp.Status)
	}
	var catalog openAIModelsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&catalog); err != nil {
		return hermesConfigPayload{}, fmt.Errorf("decode proxy model catalog: %w", err)
	}
	models := make(map[string]hermesCustomProviderModel, len(catalog.Data))
	for _, model := range catalog.Data {
		if model.ID == "" || model.Metadata.ContextLength < 1 {
			return hermesConfigPayload{}, errors.New("proxy model catalog contains an invalid model")
		}
		models[model.ID] = hermesCustomProviderModel{ContextLength: model.Metadata.ContextLength}
	}
	return hermesConfigPayload{
		Model: hermesModelConfig{Default: legacy.Model.Default, Provider: "custom:llm-proxy"},
		CustomProviders: []hermesCustomProvider{{
			Name: "llm-proxy", BaseURL: legacy.Model.BaseURL, APIMode: "chat_completions", Models: models,
		}},
	}, nil
}

func defaultHermesConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.hermes/config.yaml"
	}
	return home + "/.hermes/config.yaml"
}

func validateHermesConfig(remote hermesConfigPayload) error {
	if remote.Model.Default == "" || remote.Model.Provider == "" {
		return errors.New("model.default and model.provider are required")
	}
	if !strings.HasPrefix(remote.Model.Provider, "custom:") {
		return errors.New("model.provider must name a custom provider")
	}
	providerName := strings.TrimPrefix(remote.Model.Provider, "custom:")
	for _, provider := range remote.CustomProviders {
		if provider.Name != providerName {
			continue
		}
		if provider.BaseURL == "" || provider.APIMode != "chat_completions" {
			return errors.New("custom provider requires base_url and chat_completions api_mode")
		}
		model, ok := provider.Models[remote.Model.Default]
		if !ok || model.ContextLength < 1 {
			return errors.New("active model must be present with a positive context_length")
		}
		for id, model := range provider.Models {
			if strings.TrimSpace(id) == "" || model.ContextLength < 1 {
				return errors.New("custom provider models require IDs and positive context_length values")
			}
		}
		return nil
	}
	return fmt.Errorf("custom provider %q is missing", providerName)
}

func writeHermesConfig(path string, remote hermesConfigPayload) error {
	config := map[string]any{}
	if body, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(body, &config); err != nil {
			return fmt.Errorf("parse Hermes config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if shouldSyncHermesModel(config["model"], remote.Model.Provider) {
		model, ok := config["model"].(map[string]any)
		if !ok {
			model = map[string]any{}
		}
		model["default"] = remote.Model.Default
		model["provider"] = remote.Model.Provider
		delete(model, "base_url")
		delete(model, "api_key")
		delete(model, "api_mode")
		config["model"] = model
	}

	providerName := strings.TrimPrefix(remote.Model.Provider, "custom:")
	providers := make([]any, 0, len(remote.CustomProviders))
	if existing, exists := config["custom_providers"]; exists {
		var ok bool
		providers, ok = existing.([]any)
		if !ok {
			return errors.New("custom_providers must be a YAML list")
		}
	}
	updated := make([]any, 0, len(providers)+1)
	for _, provider := range providers {
		entry, ok := provider.(map[string]any)
		if !ok {
			return errors.New("custom_providers entries must be YAML mappings")
		}
		if entry["name"] == providerName {
			continue
		}
		updated = append(updated, entry)
	}
	for _, provider := range remote.CustomProviders {
		if provider.Name != providerName {
			continue
		}
		models := make(map[string]any, len(provider.Models))
		for id, details := range provider.Models {
			models[id] = map[string]any{"context_length": details.ContextLength}
		}
		updated = append(updated, map[string]any{
			"name": provider.Name, "base_url": provider.BaseURL, "api_mode": provider.APIMode, "models": models,
		})
	}
	config["custom_providers"] = updated
	body, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0600)
}

// bootstrapHermesConfig installs only the durable connection information that
// Hermes needs to query the proxy's live model catalog. It deliberately does
// not copy the proxy's models map: Hermes discovers that data from /v1/models.
// Existing user choices always win over bootstrap defaults.
func bootstrapHermesConfig(path string, remote hermesConfigPayload) (bool, error) {
	if err := validateHermesConfig(remote); err != nil {
		return false, err
	}
	config := map[string]any{}
	if body, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(body, &config); err != nil {
			return false, fmt.Errorf("parse Hermes config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	changed := false
	providerName := strings.TrimPrefix(remote.Model.Provider, "custom:")
	providers, err := hermesProviderEntries(config["custom_providers"])
	if err != nil {
		return false, err
	}
	if !hasHermesProvider(providers, providerName) {
		var source *hermesCustomProvider
		for i := range remote.CustomProviders {
			if remote.CustomProviders[i].Name == providerName {
				source = &remote.CustomProviders[i]
				break
			}
		}
		if source == nil {
			return false, fmt.Errorf("custom provider %q is missing", providerName)
		}
		providers = append(providers, map[string]any{
			"name":            source.Name,
			"base_url":        source.BaseURL,
			"api_mode":        source.APIMode,
			"discover_models": true,
		})
		config["custom_providers"] = providers
		changed = true
	}

	model, modelChanged, err := bootstrapHermesModel(config["model"], remote.Model)
	if err != nil {
		return false, err
	}
	if modelChanged {
		config["model"] = model
		changed = true
	}
	if !changed {
		return false, nil
	}
	body, err := yaml.Marshal(config)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		return false, err
	}
	return true, nil
}

func hermesProviderEntries(raw any) ([]any, error) {
	if raw == nil {
		return []any{}, nil
	}
	providers, ok := raw.([]any)
	if !ok {
		return nil, errors.New("custom_providers must be a YAML list")
	}
	for _, provider := range providers {
		if _, ok := provider.(map[string]any); !ok {
			return nil, errors.New("custom_providers entries must be YAML mappings")
		}
	}
	return providers, nil
}

func hasHermesProvider(providers []any, name string) bool {
	for _, provider := range providers {
		entry := provider.(map[string]any)
		if entry["name"] == name {
			return true
		}
	}
	return false
}

func bootstrapHermesModel(raw any, remote hermesModelConfig) (map[string]any, bool, error) {
	if raw == nil {
		return map[string]any{"provider": remote.Provider, "default": remote.Default}, true, nil
	}
	if legacy, ok := raw.(string); ok {
		if strings.TrimSpace(legacy) == "" {
			return map[string]any{"provider": remote.Provider, "default": remote.Default}, true, nil
		}
		// A legacy scalar model value is already an explicit user choice.
		return nil, false, nil
	}
	model, ok := raw.(map[string]any)
	if !ok {
		return nil, false, errors.New("model must be a YAML mapping")
	}
	provider, _ := model["provider"].(string)
	defaultModel, _ := model["default"].(string)
	provider = strings.TrimSpace(provider)
	defaultModel = strings.TrimSpace(defaultModel)

	// A configured non-proxy model is a user decision. Do not turn a partial
	// OpenAI/other-provider configuration into an invalid proxy selection.
	if provider != "" && provider != remote.Provider {
		return model, false, nil
	}
	if provider == remote.Provider && defaultModel == "" {
		model["default"] = remote.Default
		return model, true, nil
	}
	if provider == "" && defaultModel == "" {
		model["provider"] = remote.Provider
		model["default"] = remote.Default
		return model, true, nil
	}
	return model, false, nil
}

func shouldSyncHermesModel(current any, proxyProvider string) bool {
	if current == nil {
		return true
	}
	if sentinel, ok := current.(string); ok {
		return strings.TrimSpace(sentinel) == ""
	}
	model, ok := current.(map[string]any)
	if !ok {
		return false
	}
	provider, _ := model["provider"].(string)
	return provider == "" || provider == proxyProvider
}

func runGenerateConfig(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("generate-config", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	revision := flags.String("revision", "main", "Hugging Face revision containing config.json")
	maxModelLen := flags.Int("max-model-len", 0, "runtime vLLM context cap (defaults to the model-declared limit)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: vllm-proxy generate-config [--revision REVISION] [--max-model-len TOKENS] OWNER/MODEL")
	}
	repo := strings.TrimSpace(flags.Arg(0))
	if len(strings.Split(repo, "/")) != 2 || strings.Contains(repo, " ") {
		return errors.New("model repository must be OWNER/MODEL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	declared, err := fetchModelContext(ctx, http.DefaultClient, repo, *revision)
	if err != nil {
		return err
	}
	if *maxModelLen < 0 {
		return errors.New("max-model-len must be positive")
	}
	effective := declared
	if *maxModelLen > 0 {
		effective = *maxModelLen
	}
	fallback := map[string]any{"source": "huggingface", "model_max_context": declared}
	if parameters, err := fetchModelParameters(ctx, http.DefaultClient, repo); err == nil && parameters > 0 {
		fallback["total_parameters"] = parameters
	}
	return writeGeneratedConfig(output, repo, declared, effective, fallback)
}

func fetchModelContext(ctx context.Context, client *http.Client, repo, revision string) (int, error) {
	parts := strings.Split(repo, "/")
	url := "https://huggingface.co/" + parts[0] + "/" + parts[1] + "/raw/" + url.PathEscape(revision) + "/config.json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "vllm-proxy-config-generator/1")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch model config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch model config: Hugging Face returned %s", resp.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.UseNumber()
	var config map[string]any
	if err := decoder.Decode(&config); err != nil {
		return 0, fmt.Errorf("decode model config: %w", err)
	}
	contextLength, ok := modelContextLength(config)
	if !ok {
		return 0, errors.New("model config does not expose a recognized context-length field; pass a manual profile instead")
	}
	return contextLength, nil
}

func fetchModelParameters(ctx context.Context, client *http.Client, repo string) (int64, error) {
	parts := strings.Split(repo, "/")
	url := "https://huggingface.co/api/models/" + parts[0] + "/" + parts[1]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "vllm-proxy-config-generator/1")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("Hugging Face returned %s", resp.Status)
	}
	var payload struct {
		Safetensors struct {
			Total json.Number `json:"total"`
		} `json:"safetensors"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return 0, err
	}
	return payload.Safetensors.Total.Int64()
}

func modelContextLength(config map[string]any) (int, bool) {
	keys := []string{"max_position_embeddings", "max_sequence_length", "max_seq_len", "seq_length", "n_positions", "model_max_length"}
	for _, source := range []map[string]any{config, nestedMap(config, "text_config")} {
		for _, key := range keys {
			if value, ok := positiveInt(source[key]); ok && value <= 10_000_000 {
				return value, true
			}
		}
	}
	return 0, false
}

func nestedMap(values map[string]any, key string) map[string]any {
	if nested, ok := values[key].(map[string]any); ok {
		return nested
	}
	return nil
}

func positiveInt(value any) (int, bool) {
	var number int64
	var err error
	switch value := value.(type) {
	case json.Number:
		number, err = value.Int64()
	case string:
		number, err = strconv.ParseInt(value, 10, 64)
	default:
		return 0, false
	}
	return int(number), err == nil && number > 0 && int64(int(number)) == number
}

func writeGeneratedConfig(output io.Writer, repo string, declared, effective int, fallback map[string]any) error {
	_ = fallback // Runtime and model-card observations are status-owned by the proxy.
	_, err := fmt.Fprintf(output, `---
# Generated from %s/config.json without loading model weights. Review and tune vLLM flags before committing.
apiVersion: llm.cogito.dev/v1alpha1
kind: LLMModel
metadata:
  name: %s
spec:
  model:
    name: %s
    source: %s
  serving:
    backend: vllm
    displayName: %s
    maxModelLen: %d
    args:
      - --max-model-len
      - "%d"
      - --host
      - 0.0.0.0
      - --port
      - "8000"
	`, repo, modelSlug(repo), yamlQuote(repo), yamlQuote(repo), yamlQuote(repo), effective, effective)
	return err
}

func modelSlug(value string) string {
	var slug strings.Builder
	lastDash := false
	for _, char := range strings.ToLower(value) {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			slug.WriteRune(char)
			lastDash = false
		} else if !lastDash {
			slug.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(slug.String(), "-")
}

func yamlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// watchLLMResources keeps the CR-only catalog in sync.
func (p *proxy) watchLLMResources(logger *slog.Logger) {
	if p.dynamic == nil {
		return
	}
	go p.watchLLMResource(logger, llmModelGVR, "LLMModel")
	p.watchLLMResource(logger, llmOverlayGVR, "LLMModelOverlay")
}

func (p *proxy) watchLLMResource(logger *slog.Logger, gvr schema.GroupVersionResource, kind string) {
	for {
		watch, err := p.dynamic.Resource(gvr).Namespace(p.namespace).Watch(context.Background(), metav1.ListOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				p.configErrors.Add(1)
				logger.Warn("watch LLM resources", "kind", kind, "error", err)
			}
			time.Sleep(5 * time.Second)
			continue
		}
		for range watch.ResultChan() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := p.refresh(ctx); err != nil {
				p.configErrors.Add(1)
				logger.Warn("refresh LLM resources", "kind", kind, "error", err)
			}
			cancel()
			p.reconcileIfMutable(logger)
		}
	}
}

func (p *proxy) reconcileIfMutable(logger *slog.Logger) {
	if !p.readOnlyTransitions {
		go p.reconcileActiveDeployment(logger)
	}
}

// watchDeployments keeps the proxy state aligned with either configured
// runtime. Backend definitions are static proxy configuration, so ConfigMaps
// can select a backend without gaining control over arbitrary Deployments.
func (p *proxy) watchDeployments(logger *slog.Logger) {
	for _, backend := range p.backends {
		go p.watchDeployment(logger, backend)
	}
}

func (p *proxy) watchDeployment(logger *slog.Logger, backend backendConfig) {
	selector := fields.OneTermEqualSelector("metadata.name", backend.Deployment).String()
	for {
		watch, err := p.client.AppsV1().Deployments(p.namespace).Watch(context.Background(), metav1.ListOptions{FieldSelector: selector})
		if err != nil {
			p.configErrors.Add(1)
			logger.Warn("watch backend Deployment", "backend", backend.Name, "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		for range watch.ResultChan() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := p.syncActiveDeployment(ctx); err != nil {
				p.configErrors.Add(1)
				logger.Warn("sync active model from backend Deployment", "backend", backend.Name, "error", err)
			}
			cancel()
		}
	}
}

func (p *proxy) loadCRDRegistry(ctx context.Context, next *registry) error {
	if p.dynamic == nil {
		return nil
	}
	models, err := p.dynamic.Resource(llmModelGVR).Namespace(p.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list LLMModels: %w", err)
	}
	for _, object := range models.Items {
		cfg, err := parseLLMModel(object)
		if err != nil {
			return fmt.Errorf("LLMModel %s: %w", object.GetName(), err)
		}
		if _, exists := next.models[cfg.Name]; exists {
			return fmt.Errorf("duplicate LLMModel model.name %q", cfg.Name)
		}
		next.models[cfg.Name] = cfg
	}
	overlays, err := p.dynamic.Resource(llmOverlayGVR).Namespace(p.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list LLMModelOverlays: %w", err)
	}
	for _, object := range overlays.Items {
		cfg, err := parseLLMModelOverlay(object)
		if err != nil {
			return fmt.Errorf("LLMModelOverlay %s: %w", object.GetName(), err)
		}
		if _, exists := next.models[cfg.Name]; exists {
			return fmt.Errorf("LLMModelOverlay %s conflicts with base model %q", object.GetName(), cfg.Name)
		}
		if _, exists := next.overlays[cfg.Name]; exists {
			return fmt.Errorf("duplicate LLMModelOverlay name %q", cfg.Name)
		}
		if _, exists := next.models[cfg.BaseModel]; !exists {
			return fmt.Errorf("LLMModelOverlay %s references unknown base model %q", object.GetName(), cfg.BaseModel)
		}
		next.overlays[cfg.Name] = cfg
	}
	return nil
}

func parseLLMModel(object unstructured.Unstructured) (modelConfig, error) {
	name, found, err := unstructured.NestedString(object.Object, "spec", "model", "name")
	if err != nil || !found || strings.TrimSpace(name) == "" {
		return modelConfig{}, errors.New("spec.model.name is required")
	}
	source, found, err := unstructured.NestedString(object.Object, "spec", "model", "source")
	if err != nil || !found || strings.TrimSpace(source) == "" {
		return modelConfig{}, errors.New("spec.model.source is required")
	}
	backend, found, err := unstructured.NestedString(object.Object, "spec", "serving", "backend")
	if err != nil || !found || (backend != "vllm" && backend != "llama-cpp") {
		return modelConfig{}, fmt.Errorf("unsupported spec.serving.backend %q", backend)
	}
	displayName, found, err := unstructured.NestedString(object.Object, "spec", "serving", "displayName")
	if err != nil || !found || strings.TrimSpace(displayName) == "" {
		return modelConfig{}, errors.New("spec.serving.displayName is required")
	}
	maxModelLen, found, err := unstructured.NestedInt64(object.Object, "spec", "serving", "maxModelLen")
	if err != nil || !found || maxModelLen < 1 || maxModelLen > int64(^uint(0)>>1) {
		return modelConfig{}, errors.New("spec.serving.maxModelLen must be a positive integer")
	}
	args, found, err := unstructured.NestedStringSlice(object.Object, "spec", "serving", "args")
	if err != nil || !found || len(args) == 0 {
		return modelConfig{}, errors.New("spec.serving.args must be a non-empty string array")
	}
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" {
			return modelConfig{}, errors.New("spec.serving.args cannot contain empty arguments")
		}
	}
	if backend == "vllm" && (contains(args, "--model") || contains(args, "--revision") || contains(args, "--served-model-name")) {
		return modelConfig{}, errors.New("spec.serving.args must not contain --model, --revision, or --served-model-name")
	}
	if backend == "llama-cpp" && (contains(args, "-m") || contains(args, "--model") || contains(args, "--alias")) {
		return modelConfig{}, errors.New("spec.serving.args must not contain -m, --model, or --alias")
	}
	cfg := modelConfig{Name: name, ModelSource: source, Backend: backend, DisplayName: displayName, MaxModelLen: int(maxModelLen), Args: args, Created: object.GetCreationTimestamp().Time, Source: "crd/" + object.GetName()}
	if revision, found, _ := unstructured.NestedString(object.Object, "spec", "model", "revision"); found && revision != "" {
		if !isCommitSHA(revision) {
			return modelConfig{}, fmt.Errorf("spec.model.revision %q must be an immutable commit SHA", revision)
		}
		artifact, _, _ := unstructured.NestedString(object.Object, "spec", "model", "artifactRepository")
		if artifact == "" {
			artifact = source
		}
		cfg.Cache = cacheSpec{Kind: "huggingface-hub", RepoID: artifact, Revision: revision}
		if expectedSize, found, _ := unstructured.NestedString(object.Object, "spec", "artifact", "expectedSize"); found && expectedSize != "" {
			quantity, err := resource.ParseQuantity(expectedSize)
			if err != nil || quantity.Value() < 1 {
				return modelConfig{}, fmt.Errorf("spec.artifact.expectedSize %q must be a positive quantity", expectedSize)
			}
			cfg.Cache.Size = quantity.Value()
		}
		if backend == "llama-cpp" {
			cfg.Cache.Kind = "huggingface-files"
			files, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "artifact", "files")
			if len(files) == 0 {
				return modelConfig{}, errors.New("spec.artifact.files is required for llama-cpp")
			}
			cfg.Cache.Files = files
			target, _, _ := unstructured.NestedString(object.Object, "spec", "artifact", "materializationTarget")
			cfg.Cache.MaterializationTarget = target
		}
	}
	return cfg, nil
}

func parseLLMModelOverlay(object unstructured.Unstructured) (overlayConfig, error) {
	displayName, found, err := unstructured.NestedString(object.Object, "spec", "displayName")
	if err != nil || !found || strings.TrimSpace(displayName) == "" {
		return overlayConfig{}, errors.New("spec.displayName is required")
	}
	baseModel, found, err := unstructured.NestedString(object.Object, "spec", "baseModel")
	if err != nil || !found || strings.TrimSpace(baseModel) == "" {
		return overlayConfig{}, errors.New("spec.baseModel is required")
	}
	defaults, found, err := unstructured.NestedFieldCopy(object.Object, "spec", "requestDefaults")
	if err != nil || !found {
		return overlayConfig{}, errors.New("spec.requestDefaults is required")
	}
	raw, err := json.Marshal(defaults)
	if err != nil {
		return overlayConfig{}, fmt.Errorf("encode spec.requestDefaults: %w", err)
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return overlayConfig{}, errors.New("spec.requestDefaults must be a JSON object")
	}
	if _, exists := values["model"]; exists {
		return overlayConfig{}, errors.New("spec.requestDefaults must not set model")
	}
	return overlayConfig{Name: object.GetName(), DisplayName: displayName, BaseModel: baseModel, Created: object.GetCreationTimestamp().Time, RequestDefaults: raw, Source: "crd/" + object.GetName()}, nil
}

func (p *proxy) refresh(ctx context.Context) error {
	next := registry{
		models:   make(map[string]modelConfig),
		overlays: make(map[string]overlayConfig),
	}
	if err := p.loadCRDRegistry(ctx, &next); err != nil {
		return err
	}
	status, err := p.client.CoreV1().ConfigMaps(p.namespace).Get(ctx, modelStatusName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get model status ConfigMap: %w", err)
	}
	statusData := map[string]string{}
	if err == nil {
		statusData = status.Data
	}
	for name, cfg := range next.models {
		if value := statusData[statusDataKey(cfg.Source, ".runtime_metadata.json")]; value != "" {
			if !json.Valid([]byte(value)) {
				return fmt.Errorf("invalid runtime metadata for %s", cfg.Source)
			}
			cfg.Runtime = json.RawMessage(value)
		}
		if value := statusData[statusDataKey(cfg.Source, ".model_card_metadata.json")]; value != "" {
			if !json.Valid([]byte(value)) {
				return fmt.Errorf("invalid model-card metadata for %s", cfg.Source)
			}
			cfg.Fallback = json.RawMessage(value)
		}
		next.models[name] = cfg
	}
	p.stateMu.Lock()
	p.registry = next
	p.ready = true
	p.stateMu.Unlock()
	if err := p.syncActiveDeployment(ctx); err != nil {
		return err
	}
	p.stateMu.RLock()
	activeConfig, ok := p.registry.models[p.active]
	p.stateMu.RUnlock()
	if ok && len(activeConfig.Runtime) == 0 && p.backendHealthy(ctx) {
		if err := p.persistRuntimeMetadata(ctx, activeConfig); err != nil {
			return fmt.Errorf("persist active runtime metadata: %w", err)
		}
	}
	return nil
}

func (p *proxy) syncActiveDeployment(ctx context.Context) error {
	var activeBackend *backendConfig
	var model string
	backends := p.backends
	if len(backends) == 0 {
		backends = map[string]backendConfig{"vllm": {Name: "vllm", Deployment: p.deployment, Container: p.container, URL: p.backend}}
	}
	for _, backend := range backends {
		deployment, err := p.client.AppsV1().Deployments(p.namespace).Get(ctx, backend.Deployment, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s backend Deployment: %w", backend.Name, err)
		}
		if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas != 1 {
			continue
		}
		if activeBackend != nil {
			return errors.New("multiple LLM backends are active")
		}
		candidate := deployment.Spec.Template.Annotations[activeModelAnno]
		if candidate == "" {
			return fmt.Errorf("active %s backend has no model annotation", backend.Name)
		}
		selected := backend
		activeBackend, model = &selected, candidate
	}
	if activeBackend == nil {
		return nil
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.transitioning {
		return nil
	}
	cfg, ok := p.registry.models[model]
	if !ok {
		return fmt.Errorf("active model %q is not configured", model)
	}
	configuredBackend := cfg.Backend
	if configuredBackend == "" {
		configuredBackend = "vllm"
	}
	if configuredBackend != activeBackend.Name {
		return fmt.Errorf("active %s backend is annotated with %q configured for %s", activeBackend.Name, model, configuredBackend)
	}
	if p.active != model {
		p.active = model
		p.activeSince = time.Now()
	}
	p.backend, p.backendName = activeBackend.URL, activeBackend.Name
	return nil
}

func isCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func effectiveVLLMArgs(cfg modelConfig) []string {
	args := make([]string, 0, len(cfg.Args)+6)
	args = append(args, "--model", cfg.ModelSource)
	if cfg.Cache.Revision != "" {
		args = append(args, "--revision", cfg.Cache.Revision)
	}
	args = append(args, "--served-model-name", cfg.Name)
	return append(args, cfg.Args...)
}

func effectiveArgs(cfg modelConfig) []string {
	if cfg.Backend == "llama-cpp" {
		args := make([]string, 0, len(cfg.Args)+4)
		args = append(args, "-m", cfg.ModelSource, "--alias", cfg.Name)
		return append(args, cfg.Args...)
	}
	return effectiveVLLMArgs(cfg)
}

func (p *proxy) backendFor(cfg modelConfig) (backendConfig, error) {
	name := cfg.Backend
	if name == "" {
		name = "vllm"
	}
	if backend, ok := p.backends[name]; ok {
		return backend, nil
	}
	// Keep direct unit-test construction compatible with the original single
	// vLLM backend.
	if name == "vllm" {
		return backendConfig{Name: "vllm", Deployment: p.deployment, Container: p.container, URL: p.backend}, nil
	}
	return backendConfig{}, fmt.Errorf("backend %q is not configured", name)
}

func deploymentNeedsActivation(deployment *appsv1.Deployment, container string, cfg modelConfig) bool {
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		return true
	}
	want := effectiveArgs(cfg)
	for _, candidate := range deployment.Spec.Template.Spec.Containers {
		if candidate.Name != container {
			continue
		}
		if len(candidate.Args) != len(want) {
			return true
		}
		for i := range want {
			if candidate.Args[i] != want[i] {
				return true
			}
		}
		return false
	}
	return true
}

// reconcileActiveDeployment materializes the active model ConfigMap into the
// Helm-managed Deployment. Helm owns the pod template; Switchboard owns only
// the selected model arguments and replica count.
func (p *proxy) reconcileActiveDeployment(logger *slog.Logger) {
	if p.readOnlyTransitions {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.transitionLimit)
	defer cancel()

	p.stateMu.RLock()
	cfg, ok := p.registry.models[p.active]
	p.stateMu.RUnlock()
	if !ok {
		return
	}

	backend, err := p.backendFor(cfg)
	if err != nil {
		p.configErrors.Add(1)
		logger.Warn("resolve active model backend", "model", cfg.Name, "error", err)
		return
	}
	deployment, err := p.client.AppsV1().Deployments(p.namespace).Get(ctx, backend.Deployment, metav1.GetOptions{})
	if err != nil {
		p.configErrors.Add(1)
		logger.Warn("get backend Deployment for active-model reconciliation", "backend", backend.Name, "error", err)
		return
	}
	if !deploymentNeedsActivation(deployment, backend.Container, cfg) {
		return
	}

	p.stateMu.Lock()
	if p.active != cfg.Name {
		p.stateMu.Unlock()
		return
	}
	if p.transitioning {
		p.reconcilePending = true
		transitionCancel := p.transitionCancel
		p.stateMu.Unlock()
		if transitionCancel != nil {
			transitionCancel()
		}
		return
	}
	p.transitioning = true
	p.transitionModel = cfg.Name
	p.transitionCancel = cancel
	p.stateMu.Unlock()
	defer func() {
		p.stateMu.Lock()
		p.transitioning = false
		p.transitionModel = ""
		p.transitionCancel = nil
		pending := p.reconcilePending
		p.reconcilePending = false
		p.stateMu.Unlock()
		if pending {
			go p.reconcileActiveDeployment(logger)
		}
	}()

	if err := p.transition(ctx, cfg); err != nil {
		if !errors.Is(err, context.Canceled) {
			p.configErrors.Add(1)
			logger.Warn("reconcile active model", "model", cfg.Name, "backend", cfg.Backend, "error", err)
		}
	}
}

func (p *proxy) models(w http.ResponseWriter, _ *http.Request) {
	p.stateMu.RLock()
	data := make([]map[string]any, 0, len(p.registry.models)+len(p.registry.overlays))
	for _, cfg := range p.registry.models {
		data = append(data, modelCard(cfg))
	}
	for _, overlay := range p.registry.overlays {
		data = append(data, overlayModelCard(overlay, p.registry.models[overlay.BaseModel]))
	}
	p.stateMu.RUnlock()
	sort.Slice(data, func(i, j int) bool { return data[i]["id"].(string) < data[j]["id"].(string) })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (p *proxy) model(w http.ResponseWriter, r *http.Request) {
	p.stateMu.RLock()
	cfg, ok := p.registry.models[r.PathValue("id")]
	overlay, isOverlay := p.registry.overlays[r.PathValue("id")]
	base := p.registry.models[overlay.BaseModel]
	p.stateMu.RUnlock()
	if !ok && !isOverlay {
		openAIError(w, http.StatusNotFound, "model_not_found", "The requested model is not configured.")
		return
	}
	if isOverlay {
		writeJSON(w, http.StatusOK, overlayModelCard(overlay, base))
		return
	}
	writeJSON(w, http.StatusOK, modelCard(cfg))
}

func modelCard(cfg modelConfig) map[string]any {
	card := map[string]any{"id": cfg.Name, "object": "model", "created": cfg.Created.Unix(), "owned_by": "vllm-proxy"}
	metadata := map[string]any{"context_length": cfg.MaxModelLen, "source": "manual_config", "backend": cfg.Backend}
	if cfg.Source != "" {
		metadata["config_source"] = cfg.Source
	}
	mergeJSONMetadata(metadata, cfg.Fallback)
	mergeJSONMetadata(metadata, cfg.Runtime)
	card["metadata"] = metadata
	return card
}

func overlayModelCard(overlay overlayConfig, base modelConfig) map[string]any {
	card := map[string]any{"id": overlay.Name, "object": "model", "created": overlay.Created.Unix(), "owned_by": "vllm-proxy"}
	metadata := map[string]any{
		"context_length": base.MaxModelLen,
		"source":         "model_overlay",
		"overlay":        true,
		"base_model":     overlay.BaseModel,
	}
	if overlay.Source != "" {
		metadata["config_source"] = overlay.Source
	}
	mergeJSONMetadata(metadata, base.Fallback)
	mergeJSONMetadata(metadata, base.Runtime)
	// Overlay identity must not be masked by base model metadata.
	metadata["overlay"] = true
	metadata["base_model"] = overlay.BaseModel
	card["metadata"] = metadata
	return card
}

func mergeJSONMetadata(destination map[string]any, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return
	}
	for key, value := range values {
		destination[key] = value
	}
}

func (p *proxy) inference(w http.ResponseWriter, r *http.Request) {
	var requested string
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.maxBody))
		if err != nil {
			openAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body is too large.")
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var header struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &header) == nil {
			requested = header.Model
		}
		if overlay, ok := p.overlay(requested); ok {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
				openAIError(w, http.StatusBadRequest, "invalid_request_error", "model overlays are supported only by POST /v1/chat/completions")
				return
			}
			body, err = applyOverlay(body, overlay)
			if err != nil {
				openAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			requested = overlay.BaseModel
		}
	}
	if requested != "" {
		if err := p.ensureActive(r.Context(), requested); err != nil {
			p.respondTransitionError(w, err)
			return
		}
	} else if !p.isAvailable() {
		p.respondTransitionError(w, errBackendUnavailable)
		return
	}
	p.reverseProxy().ServeHTTP(w, r)
}

func (p *proxy) overlay(name string) (overlayConfig, bool) {
	p.stateMu.RLock()
	overlay, ok := p.registry.overlays[name]
	p.stateMu.RUnlock()
	return overlay, ok
}

func applyOverlay(body []byte, overlay overlayConfig) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, errors.New("overlay requests must contain a JSON object")
	}
	var defaults map[string]any
	if err := json.Unmarshal(overlay.RequestDefaults, &defaults); err != nil {
		return nil, fmt.Errorf("decode overlay request defaults: %w", err)
	}
	mergeDefaults(request, defaults)
	request["model"] = overlay.BaseModel
	return json.Marshal(request)
}

// mergeDefaults recursively fills omitted request fields. Explicit client
// values, including nested chat_template_kwargs, take precedence.
func mergeDefaults(request, defaults map[string]any) {
	for key, defaultValue := range defaults {
		requestValue, exists := request[key]
		if !exists {
			request[key] = defaultValue
			continue
		}
		requestMap, requestIsMap := requestValue.(map[string]any)
		defaultMap, defaultIsMap := defaultValue.(map[string]any)
		if requestIsMap && defaultIsMap {
			mergeDefaults(requestMap, defaultMap)
		}
	}
}

var (
	errTransitioning               = errors.New("model transition in progress")
	errBackendUnavailable          = errors.New("no active vLLM backend")
	errDeploymentMutationsDisabled = errors.New("model transitions are disabled")
)

func (p *proxy) ensureActive(ctx context.Context, requested string) error {
	p.stateMu.Lock()
	cfg, exists := p.registry.models[requested]
	if !exists {
		p.stateMu.Unlock()
		return fmt.Errorf("unknown model %q", requested)
	}
	if p.readOnlyTransitions {
		p.stateMu.Unlock()
		statusModel, phase, err := p.operatorTransitionState(ctx)
		if err != nil {
			return err
		}
		if statusModel != cfg.Name || phase != "Stable" {
			if err := p.requestOperatorTransition(ctx, cfg.Name); err != nil {
				return err
			}
			if err := p.waitForOperatorTransition(ctx, cfg.Name); err != nil {
				return err
			}
		}
		return p.syncActiveDeployment(ctx)
	}
	if p.transitioning {
		if p.transitionModel == requested {
			p.stateMu.Unlock()
			return errTransitioning
		}
		// A request for another model supersedes the model currently starting.
		// Keep active as the desired model so the queued reconciliation starts it
		// as soon as the canceled rollout releases the transition lock.
		if p.active != requested {
			p.active = requested
		}
		p.reconcilePending = true
		transitionCancel := p.transitionCancel
		p.stateMu.Unlock()
		if transitionCancel != nil {
			transitionCancel()
		}
		return errTransitioning
	}
	if p.active == requested {
		p.stateMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.transitioning = true
	p.transitionModel = cfg.Name
	p.transitionCancel = cancel
	p.stateMu.Unlock()

	defer func() {
		p.stateMu.Lock()
		p.transitioning = false
		p.transitionModel = ""
		p.transitionCancel = nil
		pending := p.reconcilePending
		p.reconcilePending = false
		p.stateMu.Unlock()
		if pending {
			p.reconcileIfMutable(slog.Default())
		}
	}()
	started := time.Now()
	if err := p.transition(ctx, cfg); err != nil {
		return err
	}
	p.switchesTotal.Add(1)
	p.lastSwitch.Store(time.Since(started).Nanoseconds())
	p.stateMu.Lock()
	p.active = cfg.Name
	p.activeSince = time.Now()
	p.stateMu.Unlock()
	return nil
}

// requestOperatorTransition keeps the proxy read-only with respect to
// workloads. It changes only the desired LLMActiveModel; the operator remains
// the sole component that mutates backend Deployments.
func (p *proxy) requestOperatorTransition(ctx context.Context, modelName string) error {
	if p.dynamic == nil {
		return errDeploymentMutationsDisabled
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]string{"modelName": modelName}})
	if err != nil {
		return err
	}
	if _, err := p.dynamic.Resource(activeModelGVR).Namespace(p.namespace).Patch(ctx, activeModelName, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "llm-proxy"}); err != nil {
		return fmt.Errorf("request operator transition to %q: %w", modelName, err)
	}
	return nil
}

// waitForOperatorTransition preserves the proxy's synchronous model-selection
// contract while leaving workload mutation to the operator. A request holds
// until the ActiveModel controller reports the selected model stable.
func (p *proxy) waitForOperatorTransition(parent context.Context, modelName string) error {
	ctx, cancel := context.WithTimeout(parent, p.transitionLimit)
	defer cancel()
	ticker := time.NewTicker(backendProbeWait)
	defer ticker.Stop()
	for {
		statusModel, phase, err := p.operatorTransitionState(ctx)
		if err != nil {
			return err
		}
		if statusModel == modelName && phase == "Stable" {
			return nil
		}
		if statusModel == modelName && phase == "Failed" {
			return fmt.Errorf("operator transition to %q failed", modelName)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for operator transition to %q: %w", modelName, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (p *proxy) operatorTransitionState(ctx context.Context) (string, string, error) {
	active, err := p.dynamic.Resource(activeModelGVR).Namespace(p.namespace).Get(ctx, activeModelName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("read operator ActiveModel: %w", err)
	}
	modelName, _, _ := unstructured.NestedString(active.Object, "status", "modelName")
	phase, _, _ := unstructured.NestedString(active.Object, "status", "phase")
	return modelName, phase, nil
}

func (p *proxy) transition(parent context.Context, cfg modelConfig) error {
	if p.readOnlyTransitions {
		return errDeploymentMutationsDisabled
	}
	ctx, cancel := context.WithTimeout(parent, p.transitionLimit)
	defer cancel()
	target, err := p.backendFor(cfg)
	if err != nil {
		return err
	}
	// Verify the selected runtime exists before disrupting the active one.
	// This keeps a configuration or Helm reconciliation failure from taking
	// the serving backend offline.
	if _, err := p.client.AppsV1().Deployments(p.namespace).Get(ctx, target.Deployment, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("get %s backend Deployment: %w", target.Name, err)
	}
	p.stateMu.RLock()
	currentName := p.backendName
	current, currentOK := p.backends[currentName]
	p.stateMu.RUnlock()
	if currentOK {
		if _, err := p.client.AppsV1().Deployments(p.namespace).Patch(ctx, current.Deployment, types.StrategicMergePatchType, []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("scale down %s backend: %w", current.Name, err)
		}
		for {
			old, err := p.client.AppsV1().Deployments(p.namespace).Get(ctx, current.Deployment, metav1.GetOptions{})
			if err == nil && old.Status.Replicas == 0 && old.Status.AvailableReplicas == 0 {
				break
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("wait for %s backend to stop: %w", current.Name, err)
			}
			time.Sleep(backendProbeWait)
		}
	}
	if err := p.ensureCached(ctx, cfg); err != nil {
		return err
	}
	patchedAt := time.Now()
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": 1, "template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{activeModelAnno: cfg.Name, switchedAtAnno: time.Now().UTC().Format(time.RFC3339Nano)}},
		"spec":     map[string]any{"containers": []map[string]any{{"name": target.Container, "args": effectiveArgs(cfg)}}},
	}}})
	if err != nil {
		return err
	}
	deployment, err := p.client.AppsV1().Deployments(p.namespace).Patch(ctx, target.Deployment, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch %s backend Deployment: %w", target.Name, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for vLLM rollout: %w", err)
		}
		current, err := p.client.AppsV1().Deployments(p.namespace).Get(ctx, target.Deployment, metav1.GetOptions{})
		if err == nil && current.Status.ObservedGeneration >= deployment.Generation && current.Status.UpdatedReplicas == 1 && current.Status.AvailableReplicas == 1 {
			if p.backendHealthyAt(ctx, target.URL) {
				p.lastStart.Store(time.Since(patchedAt).Nanoseconds())
				p.stateMu.Lock()
				p.backend = target.URL
				p.backendName = target.Name
				p.stateMu.Unlock()
				if err := p.persistRuntimeMetadata(ctx, cfg); err != nil {
					p.configErrors.Add(1)
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(backendProbeWait):
		}
	}
}

func (p *proxy) ensureCached(ctx context.Context, cfg modelConfig) error {
	if p.cacheManager == nil {
		return nil
	}
	if cfg.Cache.Kind == "" {
		return fmt.Errorf("model %q has no cache.json", cfg.Name)
	}
	body, err := json.Marshal(map[string]any{"model": cfg.Name, "backend": cfg.Backend, "cache": cfg.Cache})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cacheManager.ResolveReference(&url.URL{Path: "/v1/ensure"}).String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Cache hydration may copy tens of gigabytes from the NAS. The request
	// context already carries the model-transition deadline; do not reuse the
	// short timeout intended for backend health and metadata requests.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("ensure cached model %q: %w", cfg.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ensure cached model %q: %s: %s", cfg.Name, resp.Status, strings.TrimSpace(string(message)))
	}
	switch resp.Header.Get("X-LLM-Cache-Result") {
	case "hot":
		p.cacheHotHits.Add(1)
	case "cold":
		p.cacheColdHits.Add(1)
	case "external":
		p.cacheExternal.Add(1)
	}
	return nil
}

// sweepCache keeps the hot volumes below their high-water mark even when no
// model switch occurs. The manager receives the active artifact key and never
// evicts it.
func (p *proxy) sweepCache(logger *slog.Logger) {
	if p.cacheManager == nil {
		return
	}
	ticker := time.NewTicker(durationEnv("CACHE_SWEEP_INTERVAL", defaultSweep))
	defer ticker.Stop()
	for range ticker.C {
		p.stateMu.RLock()
		cfg, ok := p.registry.models[p.active]
		transitioning := p.transitioning
		p.stateMu.RUnlock()
		if !ok || transitioning || cfg.Cache.Kind == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.transitionLimit)
		err := p.sweepCached(ctx, cfg)
		cancel()
		if err != nil {
			logger.Warn("sweep model cache", "model", cfg.Name, "error", err)
		}
	}
}

func (p *proxy) sweepCached(ctx context.Context, cfg modelConfig) error {
	body, err := json.Marshal(cacheRequest{Model: cfg.Name, Backend: cfg.Backend, Cache: cfg.Cache})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cacheManager.ResolveReference(&url.URL{Path: "/v1/sweep"}).String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

func (p *proxy) persistRuntimeMetadata(ctx context.Context, cfg modelConfig) error {
	metadata, err := p.collectRuntimeMetadata(ctx, cfg)
	if err != nil {
		return err
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	data := map[string]string{statusDataKey(cfg.Source, ".runtime_metadata.json"): string(body)}
	status, err := p.client.CoreV1().ConfigMaps(p.namespace).Get(ctx, modelStatusName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("read model status ConfigMap: %w", err)
	}
	if apierrors.IsNotFound(err) || status == nil {
		status = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: modelStatusName, Namespace: p.namespace, Labels: map[string]string{"llm.cogito.dev/model-status": "true"}}, Data: map[string]string{}}
	}
	if status.Data[statusDataKey(cfg.Source, ".model_card_metadata.json")] == "" {
		if fallback, err := p.modelCardFallback(ctx, cfg); err == nil {
			data[statusDataKey(cfg.Source, ".model_card_metadata.json")] = fallback
		}
	}
	if apierrors.IsNotFound(err) {
		for key, value := range data {
			status.Data[key] = value
		}
		if _, err := p.client.CoreV1().ConfigMaps(p.namespace).Create(ctx, status, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create model status ConfigMap: %w", err)
		}
		return nil
	}
	patch, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return err
	}
	_, err = p.client.CoreV1().ConfigMaps(p.namespace).Patch(ctx, modelStatusName, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "vllm-proxy"})
	if err != nil {
		return fmt.Errorf("persist model status: %w", err)
	}
	return nil
}

// statusDataKey converts an immutable catalog source into a valid ConfigMap
// data key. Kubernetes ConfigMap keys cannot contain '/', so a CR source such
// as crd/<resource> is stored as crd__<resource>; the metadata payload retains
// the original source value.
func statusDataKey(source, suffix string) string {
	return strings.ReplaceAll(source, "/", "__") + suffix
}

func (p *proxy) modelCardFallback(ctx context.Context, cfg modelConfig) (string, error) {
	repo := cfg.ModelSource
	if len(strings.Split(repo, "/")) != 2 {
		return "", errors.New("model repository is not OWNER/MODEL")
	}
	metadata := map[string]any{"source": "huggingface", "model_max_context": cfg.MaxModelLen}
	if declared, err := fetchModelContext(ctx, p.httpClient, repo, "main"); err == nil {
		metadata["model_max_context"] = declared
	}
	if parameters, err := fetchModelParameters(ctx, p.httpClient, repo); err == nil && parameters > 0 {
		metadata["total_parameters"] = parameters
	}
	body, err := json.Marshal(metadata)
	return string(body), err
}

func (p *proxy) collectRuntimeMetadata(ctx context.Context, cfg modelConfig) (runtimeMetadata, error) {
	metadata := runtimeMetadata{
		SchemaVersion:   1,
		Source:          "vllm_runtime",
		ObservedAt:      time.Now().UTC(),
		ModelName:       cfg.Name,
		ContextLength:   cfg.MaxModelLen,
		LaunchArguments: launchArguments(effectiveArgs(cfg)),
	}
	metadata.MaxConcurrentRequests, _ = strconv.Atoi(metadata.LaunchArguments["--max-num-seqs"])
	metrics, err := p.backendText(ctx, "/metrics")
	if err == nil {
		metadata.KVCache = cacheConfigInfo(metrics)
	}
	models, err := p.backendModels(ctx)
	if err == nil {
		metadata.ServedModelIDs = models
	}
	return metadata, nil
}

func (p *proxy) backendText(ctx context.Context, endpoint string) (string, error) {
	u := *p.backend
	u.Path = endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("backend %s returned %s", endpoint, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(body), err
}

func (p *proxy) backendModels(ctx context.Context) ([]string, error) {
	body, err := p.backendText(ctx, "/v1/models")
	if err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		if model.ID != "" {
			ids = append(ids, model.ID)
		}
	}
	return ids, nil
}

func launchArguments(args []string) map[string]string {
	values := map[string]string{}
	for index := 0; index < len(args); index++ {
		if !strings.HasPrefix(args[index], "--") {
			continue
		}
		if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			values[args[index]] = args[index+1]
			index++
		} else {
			values[args[index]] = "true"
		}
	}
	return values
}

func cacheConfigInfo(metrics string) map[string]string {
	scanner := bufio.NewScanner(strings.NewReader(metrics))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "vllm:cache_config_info{") {
			continue
		}
		start, end := strings.IndexByte(line, '{'), strings.LastIndex(line, "}")
		if start < 0 || end <= start {
			return nil
		}
		return prometheusLabels(line[start+1 : end])
	}
	return nil
}

func prometheusLabels(encoded string) map[string]string {
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
		if end == len(encoded) {
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

func (p *proxy) backendHealthy(ctx context.Context) bool {
	p.stateMu.RLock()
	backend := p.backend
	p.stateMu.RUnlock()
	return p.backendHealthyAt(ctx, backend)
}

func (p *proxy) backendHealthyAt(ctx context.Context, backend *url.URL) bool {
	if backend == nil {
		return false
	}
	u := *backend
	u.Path = "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *proxy) reverseProxy() *httputil.ReverseProxy {
	p.stateMu.RLock()
	backend := p.backend
	p.stateMu.RUnlock()
	rp := httputil.NewSingleHostReverseProxy(backend)
	rp.FlushInterval = -1
	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		openAIError(w, http.StatusBadGateway, "api_error", "LLM backend communication failed: "+err.Error())
	}
	return rp
}

func (p *proxy) isAvailable() bool {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.active != "" && !p.transitioning
}

func (p *proxy) respondTransitionError(w http.ResponseWriter, err error) {
	if strings.HasPrefix(err.Error(), "unknown model") {
		openAIError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	if errors.Is(err, errDeploymentMutationsDisabled) {
		openAIError(w, http.StatusConflict, "server_error", err.Error())
		return
	}
	if errors.Is(err, errTransitioning) || errors.Is(err, errBackendUnavailable) {
		w.Header().Set("Retry-After", "15")
		openAIError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	openAIError(w, http.StatusGatewayTimeout, "api_error", err.Error())
}

func (p *proxy) healthz(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
func (p *proxy) readyz(w http.ResponseWriter, _ *http.Request) {
	p.stateMu.RLock()
	ready := p.ready
	p.stateMu.RUnlock()
	if !ready {
		http.Error(w, "model registry is not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (p *proxy) metrics(w http.ResponseWriter, _ *http.Request) {
	p.stateMu.RLock()
	active, transitioning, activeSince := p.active, p.transitioning, p.activeSince
	p.stateMu.RUnlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if active != "" {
		fmt.Fprintf(w, "vllm_proxy_active_model_info{model_name=%q} 1\n", active)
	}
	fmt.Fprintf(w, "vllm_proxy_transitioning %d\n", boolNumber(transitioning))
	fmt.Fprintf(w, "vllm_proxy_switches_total %d\n", p.switchesTotal.Load())
	fmt.Fprintf(w, "vllm_proxy_config_errors_total %d\n", p.configErrors.Load())
	fmt.Fprintf(w, "vllm_proxy_model_cache_hits_total{source=\"hot\"} %d\n", p.cacheHotHits.Load())
	fmt.Fprintf(w, "vllm_proxy_model_cache_hits_total{source=\"cold\"} %d\n", p.cacheColdHits.Load())
	fmt.Fprintf(w, "vllm_proxy_model_cache_hits_total{source=\"external\"} %d\n", p.cacheExternal.Load())
	fmt.Fprintf(w, "vllm_proxy_last_switch_duration_seconds %.6f\n", float64(p.lastSwitch.Load())/float64(time.Second))
	fmt.Fprintf(w, "vllm_proxy_last_startup_duration_seconds %.6f\n", float64(p.lastStart.Load())/float64(time.Second))
	if !activeSince.IsZero() {
		fmt.Fprintf(w, "vllm_proxy_active_model_uptime_seconds %.6f\n", time.Since(activeSince).Seconds())
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func openAIError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": typ}})
}
func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func boolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(os.Getenv(key)); err == nil && value > 0 {
		return value
	}
	return fallback
}
func int64Env(key string, fallback int64) int64 {
	if value, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && value > 0 {
		return value
	}
	return fallback
}
func mustNamespace() string {
	body, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "home-infra"
	}
	return strings.TrimSpace(string(body))
}

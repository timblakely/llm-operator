/*
 * Copyright 2025.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cogitodevv1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackendType identifies the serving runtime.
// +kubebuilder:validation:Enum=vllm;sglang;llama-cpp
type BackendType string

const (
	BackendVLLM     BackendType = "vllm"
	BackendSGLang   BackendType = "sglang"
	BackendLlamaCpp BackendType = "llama-cpp"
)

// LLMModelPhase describes the high-level lifecycle state of a model.
// +kubebuilder:validation:Enum=Pending;Ready;Active;Transitioning;Failed
type LLMModelPhase string

const (
	ModelPhasePending       LLMModelPhase = "Pending"
	ModelPhaseReady         LLMModelPhase = "Ready"
	ModelPhaseActive        LLMModelPhase = "Active"
	ModelPhaseTransitioning LLMModelPhase = "Transitioning"
	ModelPhaseFailed        LLMModelPhase = "Failed"
)

// ModelProtectionFinalizer prevents deletion of the active model.
const ModelProtectionFinalizer = "llm.cogito.dev/model-protection"

// LLMModelSpec defines the desired state of LLMModel.
// +kubebuilder:validation:XValidation:rule="self.serving.backend in ['vllm', 'sglang', 'llama-cpp']",message="backend must be 'vllm', 'sglang', or 'llama-cpp'"
// +kubebuilder:validation:XValidation:rule="has(self.model) && has(self.model.name) && size(self.model.name) > 0",message="model.name is required"
// +kubebuilder:validation:XValidation:rule="has(self.model) && has(self.model.source) && size(self.model.source) > 0",message="model.source is required"
// +kubebuilder:object:generate=true
type LLMModelSpec struct {
	// Model identity.
	Model LLMModelRef `json:"model"`

	// Artifact overrides for model cache materialization. Hub-backed vLLM and
	// SGLang models derive their immutable snapshot from model.name and
	// model.revision when this is omitted. Use this for file-layout-specific
	// artifacts such as GGUF materialization targets and expected file lists.
	Artifact *ArtifactSpec `json:"artifact,omitempty"`

	// Serving configuration.
	Serving ServingSpec `json:"serving"`

	// Which backend deployment to use. If omitted, defaults to the backend
	// matching the serving.backend value from the cluster's LLMBackend registry.
	BackendRef *corev1.LocalObjectReference `json:"backendRef,omitempty"`
}

// LLMModelRef identifies a model source.
// +kubebuilder:object:generate=true
type LLMModelRef struct {
	// Name is the canonical model identifier (e.g. "google/gemma-4-31B-it-qat-w4a16-ct").
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._/-]+$`
	Name string `json:"name"`

	// Source is the artifact location: HF repo ID for vLLM/SGLang, local path for llama.cpp.
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`

	// Revision is an immutable commit SHA or tag.
	Revision string `json:"revision,omitempty"`

	// ArtifactRepository is the upstream repo for GGUF files (optional, for llama-cpp).
	ArtifactRepository string `json:"artifactRepository,omitempty"`
}

// ArtifactSpec describes model artifact download/cache configuration.
// +kubebuilder:object:generate=true
type ArtifactSpec struct {
	// ExpectedSize is a human-readable size estimate (e.g. "60Gi").
	ExpectedSize string `json:"expectedSize,omitempty"`

	// Files lists individual files for GGUF/file-based artifacts.
	Files []string `json:"files,omitempty"`

	// MaterializationTarget is the cache-manager-owned relative directory for a
	// file-based artifact in the shared hot cache. It must be rooted at gguf/;
	// omit it only while migrating a legacy per-backend GGUF cache.
	MaterializationTarget string `json:"materializationTarget,omitempty"`
}

// ServingSpec describes the serving configuration for a model.
// +kubebuilder:object:generate=true
type ServingSpec struct {
	// Backend is the serving runtime: "vllm", "sglang", or "llama-cpp".
	// +kubebuilder:validation:Enum=vllm;sglang;llama-cpp
	Backend BackendType `json:"backend"`

	// DisplayName is a human-readable label.
	DisplayName string `json:"displayName"`

	// MaxModelLen is the maximum context length in tokens.
	// +kubebuilder:validation:Minimum=1
	MaxModelLen int `json:"maxModelLen"`

	// ToolCallParser is a portable tool-call parser selection. The selected
	// backend driver translates it to the runtime's native configuration.
	ToolCallParser string `json:"toolCallParser,omitempty"`

	// ReasoningParser is a portable reasoning parser selection. The selected
	// backend driver translates it to the runtime's native configuration.
	ReasoningParser string `json:"reasoningParser,omitempty"`

	// ChatTemplate selects a reviewed server-side chat template for this model.
	// The referenced ConfigMap must be in the same namespace as the LLMModel
	// and its selected key must match the pinned SHA-256 digest. When omitted,
	// the runtime's model-provided template is used.
	ChatTemplate *ChatTemplateSpec `json:"chatTemplate,omitempty"`

	// Args are backend-specific CLI arguments. Model name, revision, and served-model-name
	// are injected by the controller and must NOT be included here. Existing raw parser
	// flags remain supported for backwards compatibility; do not mix them with the
	// corresponding structured parser field.
	Args []string `json:"args"`
}

// ChatTemplateSpec identifies immutable, GitOps-managed server-side template
// content. Template bytes are deliberately not embedded in a CR so ordinary
// Kubernetes ConfigMap lifecycle, review, and size limits apply.
// +kubebuilder:object:generate=true
type ChatTemplateSpec struct {
	// ConfigMapKeyRef selects the same-namespace ConfigMap key containing the
	// UTF-8 Jinja template.
	ConfigMapKeyRef corev1.ConfigMapKeySelector `json:"configMapKeyRef"`

	// SHA256 is the lowercase hexadecimal SHA-256 digest of the selected
	// ConfigMap value. It prevents a mutable ConfigMap update from silently
	// changing an active model's prompt contract.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
}

// ResolvedChatTemplate identifies the template observed by the runtime.
// +kubebuilder:object:generate=true
type ResolvedChatTemplate struct {
	ConfigMapName string `json:"configMapName"`
	Key           string `json:"key"`
	SHA256        string `json:"sha256"`
}

// CacheState describes the artifact availability.
// +kubebuilder:object:generate=true
type CacheState struct {
	// Location: "hot", "cold", "external", or "unknown".
	Location string `json:"location"`

	// LastHydrated is when the artifact was last promoted to hot cache.
	LastHydrated *metav1.Time `json:"lastHydrated,omitempty"`
}

// RuntimeMetadata captures observed runtime properties after successful startup.
// +kubebuilder:object:generate=true
type RuntimeMetadata struct {
	ObservedAt        metav1.Time           `json:"observedAt"`
	ServedModelIDs    []string              `json:"servedModelIDs,omitempty"`
	ContextLength     int                   `json:"contextLength"`
	MaxConcurrentReqs int                   `json:"maxConcurrentRequests,omitempty"`
	LaunchArguments   map[string]string     `json:"launchArguments,omitempty"`
	KVCache           map[string]string     `json:"kvCache,omitempty"`
	ChatTemplate      *ResolvedChatTemplate `json:"chatTemplate,omitempty"`
}

// LLMModelStatus defines the observed state of LLMModel.
// +kubebuilder:object:generate=true
type LLMModelStatus struct {
	// Phase is the high-level lifecycle state.
	Phase LLMModelPhase `json:"phase,omitempty"`

	// Active indicates this model is currently served by a backend.
	Active bool `json:"active"`

	// CacheState describes the artifact availability.
	CacheState *CacheState `json:"cacheState,omitempty"`

	// RuntimeMetadata captures observed runtime properties after successful startup.
	RuntimeMetadata *RuntimeMetadata `json:"runtimeMetadata,omitempty"`

	// Conditions track subresource health.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled generation.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:singular=llmmodel,shortName=llmm,categories={llm}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.model.name`,description="Model name"
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.serving.backend`,description="Serving backend"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Phase"
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`,description="Currently active"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// LLMModel is the Schema for the llmmodels API.
type LLMModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMModelSpec   `json:"spec,omitempty"`
	Status LLMModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LLMModelList contains a list of LLMModel.
type LLMModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMModel{}, &LLMModelList{})
}

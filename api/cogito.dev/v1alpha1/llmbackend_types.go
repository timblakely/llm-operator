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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LLMBackendPhase indicates backend health.
// +kubebuilder:validation:Enum=Stopped;Starting;Serving;Failed
type LLMBackendPhase string

const (
	BackendPhaseStopped  LLMBackendPhase = "Stopped"
	BackendPhaseStarting LLMBackendPhase = "Starting"
	BackendPhaseServing  LLMBackendPhase = "Serving"
	BackendPhaseFailed   LLMBackendPhase = "Failed"
)

// BackendWorkloadFinalizer prevents an active workload-owning backend from
// being removed before the active model has moved elsewhere.
const BackendWorkloadFinalizer = "llm.cogito.dev/backend-workload-protection"

// GPUResourceRequirements describes GPU resources for a backend.
// +kubebuilder:object:generate=true
type GPUResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// LLMBackendSpec defines the desired state of LLMBackend.
// +kubebuilder:validation:XValidation:rule="self.type in ['vllm', 'sglang', 'llama-cpp']",message="type must be 'vllm', 'sglang', or 'llama-cpp'"
// +kubebuilder:validation:XValidation:rule="self.port > 0",message="port must be greater than 0"
// +kubebuilder:object:generate=true
type LLMBackendSpec struct {
	// Type is the backend runtime: "vllm", "sglang", or "llama-cpp".
	Type BackendType `json:"type"`

	// Workload declares the complete serving workload. The operator creates and
	// owns the Deployment and Service from this template. During migration, this
	// is mutually exclusive with the deprecated reference fields below.
	Workload *BackendWorkloadSpec `json:"workload,omitempty"`

	// DeploymentRef points to a Kubernetes Deployment that runs this backend.
	// Deprecated: use Workload. It exists only to permit a non-disruptive
	// migration of existing backends.
	DeploymentRef corev1.LocalObjectReference `json:"deploymentRef,omitempty"`

	// ContainerName is the container within the deployment that serves requests.
	// Deprecated: in workload mode use workload.containerName.
	ContainerName string `json:"containerName,omitempty"`

	// ServiceRef points to the Service that exposes this backend.
	// Deprecated: use Workload.
	ServiceRef corev1.LocalObjectReference `json:"serviceRef,omitempty"`

	// Port is the serving port on the backend container.
	// Deprecated: in workload mode use workload.service.port.
	// +kubebuilder:validation:Minimum=1
	Port int `json:"port"`

	// RuntimeClassName for GPU nodes (e.g. "nvidia").
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// GPU resources required by this backend.
	GPUResources *GPUResourceRequirements `json:"gpuResources,omitempty"`
}

// BackendWorkloadSpec is the complete Kubernetes workload owned by an
// LLMBackend. The operator supplies selectors, ownership metadata, active
// model annotations, and runtime arguments; all other Pod details belong here.
// +kubebuilder:object:generate=true
type BackendWorkloadSpec struct {
	// ContainerName identifies exactly one runtime container in Deployment.PodTemplate.
	ContainerName string `json:"containerName"`

	// Deployment configures the generated Deployment.
	Deployment BackendDeploymentSpec `json:"deployment"`

	// Service configures the generated ClusterIP Service.
	Service BackendServiceSpec `json:"service"`
}

// BackendDeploymentSpec describes the generated Deployment baseline.
// +kubebuilder:object:generate=true
type BackendDeploymentSpec struct {
	// Name is the generated Deployment name. It defaults to the LLMBackend name.
	// Specify a distinct temporary name during a Helm-to-operator migration.
	Name string `json:"name,omitempty"`

	// Replicas is the inactive baseline. The active-model controller owns the
	// runtime value and switches it between zero and one.
	// +kubebuilder:default:=0
	Replicas *int32 `json:"replicas,omitempty"`

	// Strategy is applied to the generated Deployment.
	Strategy appsv1.DeploymentStrategy `json:"strategy,omitempty"`

	// PodTemplate is the complete native Kubernetes pod template.
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`
}

// BackendServiceSpec describes the generated Service.
// +kubebuilder:object:generate=true
type BackendServiceSpec struct {
	// Name is the generated Service name. It defaults to the LLMBackend name.
	// Specify a distinct temporary name during a Helm-to-operator migration.
	Name string `json:"name,omitempty"`

	// Port is the serving port exposed by the generated ClusterIP Service.
	// +kubebuilder:validation:Minimum=1
	Port int `json:"port"`

	// PortName is the optional Service port name. It defaults to http.
	PortName string `json:"portName,omitempty"`

	// Annotations are applied to the generated Service.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// LLMBackendStatus defines the observed state of LLMBackend.
// +kubebuilder:object:generate=true
type LLMBackendStatus struct {
	// Phase indicates backend health.
	Phase LLMBackendPhase `json:"phase,omitempty"`

	// ActiveModel is the model currently served by this backend (if any).
	ActiveModel string `json:"activeModel,omitempty"`

	// ActiveModelSince is when the current model became active.
	ActiveModelSince *metav1.Time `json:"activeModelSince,omitempty"`

	// Replicas is the current replica count.
	Replicas int32 `json:"replicas"`

	// AvailableReplicas.
	AvailableReplicas int32 `json:"availableReplicas"`

	// Conditions.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled generation.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:singular=llmbackend,shortName=llmb,categories={llm}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`,description="Backend type"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Phase"
// +kubebuilder:printcolumn:name="ActiveModel",type=string,JSONPath=`.status.activeModel`,description="Active model"
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`,description="Replicas"

// LLMBackend is the Schema for the llmbackends API.
type LLMBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMBackendSpec   `json:"spec,omitempty"`
	Status LLMBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LLMBackendList contains a list of LLMBackend.
type LLMBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMBackend `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMBackend{}, &LLMBackendList{})
}

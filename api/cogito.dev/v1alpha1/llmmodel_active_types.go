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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LLMActiveModelPhase describes the transition state.
// +kubebuilder:validation:Enum=Stable;Transitioning;Failed
type LLMActiveModelPhase string

const (
	ActiveModelPhaseStable        LLMActiveModelPhase = "Stable"
	ActiveModelPhaseTransitioning LLMActiveModelPhase = "Transitioning"
	ActiveModelPhaseFailed        LLMActiveModelPhase = "Failed"
)

// LLMActiveModelSpec defines the desired state of LLMActiveModel.
// +kubebuilder:validation:XValidation:rule="self.modelName.matches('^[a-zA-Z0-9._/-]+$')",message="modelName must be a valid model identifier"
// +kubebuilder:object:generate=true
type LLMActiveModelSpec struct {
	// ModelName is the LLMModel to activate.
	ModelName string `json:"modelName"`
}

// LLMActiveModelStatus defines the observed state of LLMActiveModel.
// +kubebuilder:object:generate=true
type LLMActiveModelStatus struct {
	// ModelName is the currently active model (may differ from spec during transitions).
	ModelName string `json:"modelName"`

	// BackendType is the backend currently serving the active model.
	BackendType BackendType `json:"backendType"`

	// Phase describes the transition state.
	Phase LLMActiveModelPhase `json:"phase"`

	// TransitionFrom is the previous model (if switching).
	TransitionFrom string `json:"transitionFrom,omitempty"`

	// TransitionStarted is when the current transition began.
	TransitionStarted *metav1.Time `json:"transitionStarted,omitempty"`

	// TransitionDuration is how long the last transition took.
	TransitionDuration *metav1.Duration `json:"transitionDuration,omitempty"`

	// Conditions track health.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled generation.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:singular=llmactivemodel,shortName=llma,categories={llm}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelName`,description="Requested model"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Phase"
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.status.backendType`,description="Backend type"

// LLMActiveModel is the Schema for the llmactivemodels API.
type LLMActiveModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMActiveModelSpec   `json:"spec,omitempty"`
	Status LLMActiveModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LLMActiveModelList contains a list of LLMActiveModel.
type LLMActiveModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMActiveModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMActiveModel{}, &LLMActiveModelList{})
}

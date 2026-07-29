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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LLMModelOverlaySpec defines the desired state of LLMModelOverlay.
// +kubebuilder:object:generate=true
type LLMModelOverlaySpec struct {
	// DisplayName is a human-readable label for the overlay.
	DisplayName string `json:"displayName"`

	// BaseModel references the underlying LLMModel by canonical model name.
	// Canonical names may include a repository separator (for example,
	// "org/model").
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._/-]+$`
	BaseModel string `json:"baseModel"`

	// RequestDefaults merges into client requests before forwarding to the backend.
	// Keys not present in the client request are filled in. Client values always win.
	// Must NOT contain a "model" key (the controller sets this to the base model).
	// +kubebuilder:pruning:PreserveUnknownFields
	RequestDefaults apiextensionsv1.JSON `json:"requestDefaults"`
}

// LLMModelOverlayStatus defines the observed state of LLMModelOverlay.
// +kubebuilder:object:generate=true
type LLMModelOverlayStatus struct {
	// ResolvedBaseModel is the effective base model name after resolution.
	ResolvedBaseModel string `json:"resolvedBaseModel,omitempty"`

	// Conditions track validation.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the last reconciled generation.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:singular=llmmodeloverlay,shortName=llmo,categories={llm}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Display",type=string,JSONPath=`.spec.displayName`,description="Display name"
// +kubebuilder:printcolumn:name="BaseModel",type=string,JSONPath=`.spec.baseModel`,description="Base model"

// LLMModelOverlay is the Schema for the llmmodeloverlays API.
type LLMModelOverlay struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMModelOverlaySpec   `json:"spec,omitempty"`
	Status LLMModelOverlayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LLMModelOverlayList contains a list of LLMModelOverlay.
type LLMModelOverlayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMModelOverlay `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMModelOverlay{}, &LLMModelOverlayList{})
}

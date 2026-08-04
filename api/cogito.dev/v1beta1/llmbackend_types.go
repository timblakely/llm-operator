/*
 * Copyright 2026.
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

package cogitodevv1beta1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

// LLMBackendSpec is the workload-only v1beta1 contract. The v1alpha1
// deployment/service reference fields are deliberately absent.
// +kubebuilder:validation:XValidation:rule="self.type in ['vllm', 'sglang', 'llama-cpp']",message="type must be 'vllm', 'sglang', or 'llama-cpp'"
// +kubebuilder:object:generate=true
type LLMBackendSpec struct {
	Type cogitodevv1alpha1.BackendType `json:"type"`

	// Capacity is the normalized accelerator requirement used for placement
	// decisions and must match the runtime container's request and limit.
	Capacity *cogitodevv1alpha1.BackendCapacitySpec `json:"capacity"`

	// Workload is the complete serving workload owned by the operator.
	Workload *cogitodevv1alpha1.BackendWorkloadSpec `json:"workload"`
}

// +kubebuilder:object:generate=true
type LLMBackendStatus = cogitodevv1alpha1.LLMBackendStatus

// +kubebuilder:object:root=true
// +kubebuilder:resource:singular=llmbackend,shortName=llmb,categories={llm}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`,description="Backend type"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Phase"
// +kubebuilder:printcolumn:name="ActiveModel",type=string,JSONPath=`.status.activeModel`,description="Active model"
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`,description="Replicas"
type LLMBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LLMBackendSpec   `json:"spec"`
	Status LLMBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type LLMBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LLMBackend `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LLMBackend{}, &LLMBackendList{})
}

var _ conversion.Convertible = &LLMBackend{}

// ConvertTo converts the workload-only v1beta1 representation to the
// v1alpha1 controller hub. There are no reference-mode fields to restore.
func (src *LLMBackend) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*cogitodevv1alpha1.LLMBackend)
	if !ok {
		return fmt.Errorf("convert LLMBackend to unexpected hub type %T", dstRaw)
	}
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec.Type = src.Spec.Type
	if src.Spec.Capacity != nil {
		dst.Spec.Capacity = src.Spec.Capacity.DeepCopy()
	}
	if src.Spec.Workload != nil {
		dst.Spec.Workload = src.Spec.Workload.DeepCopy()
	}
	dst.Status = src.Status
	return nil
}

// ConvertFrom converts the v1alpha1 hub to v1beta1. Reference-mode objects
// cannot be represented in v1beta1 and are rejected before a CRD storage
// migration; this guard prevents silently discarding those fields if that
// preflight is bypassed.
func (dst *LLMBackend) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*cogitodevv1alpha1.LLMBackend)
	if !ok {
		return fmt.Errorf("convert LLMBackend from unexpected hub type %T", srcRaw)
	}
	if src.Spec.Workload == nil || src.Spec.DeploymentRef.Name != "" || src.Spec.ServiceRef.Name != "" || src.Spec.ContainerName != "" || src.Spec.Port != 0 {
		return fmt.Errorf("cannot convert reference-mode LLMBackend %q to v1beta1; migrate it to workload mode first", src.Name)
	}
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec.Type = src.Spec.Type
	if src.Spec.Capacity != nil {
		dst.Spec.Capacity = src.Spec.Capacity.DeepCopy()
	}
	dst.Spec.Workload = src.Spec.Workload.DeepCopy()
	dst.Status = src.Status
	return nil
}

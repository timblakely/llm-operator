package cogitodevv1beta1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestLLMBackendRoundTripConversion(t *testing.T) {
	src := &LLMBackend{ObjectMeta: metav1.ObjectMeta{Name: "owned"}, Spec: LLMBackendSpec{
		Type:     cogitodevv1alpha1.BackendVLLM,
		Capacity: &cogitodevv1alpha1.BackendCapacitySpec{GPUs: 2},
		Workload: &cogitodevv1alpha1.BackendWorkloadSpec{},
	}}
	hub := &cogitodevv1alpha1.LLMBackend{}
	if err := src.ConvertTo(hub); err != nil {
		t.Fatalf("convert to hub: %v", err)
	}
	got := &LLMBackend{}
	if err := got.ConvertFrom(hub); err != nil {
		t.Fatalf("convert from hub: %v", err)
	}
	if got.Name != src.Name || got.Spec.Type != src.Spec.Type || got.Spec.Capacity.GPUs != 2 || got.Spec.Workload == nil {
		t.Fatalf("round trip result = %#v", got)
	}
}

func TestLLMBackendConversionRejectsReferenceMode(t *testing.T) {
	legacy := &cogitodevv1alpha1.LLMBackend{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}, Spec: cogitodevv1alpha1.LLMBackendSpec{
		Type: cogitodevv1alpha1.BackendVLLM, DeploymentRef: corev1.LocalObjectReference{Name: "legacy"}, ContainerName: "runtime", Port: 8000,
	}}
	if err := (&LLMBackend{}).ConvertFrom(legacy); err == nil || !strings.Contains(err.Error(), "reference-mode") {
		t.Fatalf("reference-mode conversion error = %v", err)
	}
}

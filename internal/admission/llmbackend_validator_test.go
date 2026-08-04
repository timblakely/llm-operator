package admission

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestLLMBackendValidator(t *testing.T) {
	validator := LLMBackendValidator{}
	valid := workloadBackend()
	if _, err := validator.ValidateCreate(context.Background(), valid); err != nil {
		t.Fatalf("valid workload backend: %v", err)
	}

	mixed := valid.DeepCopy()
	mixed.Spec.Port = 8000
	if _, err := validator.ValidateCreate(context.Background(), mixed); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed mode error = %v", err)
	}

	args := valid.DeepCopy()
	args.Spec.Workload.Deployment.PodTemplate.Spec.Containers[0].Args = []string{"--model", "wrong-place"}
	if _, err := validator.ValidateCreate(context.Background(), args); err == nil || !strings.Contains(err.Error(), "must not set args") {
		t.Fatalf("runtime args error = %v", err)
	}

	missing := valid.DeepCopy()
	missing.Spec.Workload.ContainerName = "missing"
	if _, err := validator.ValidateCreate(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("container error = %v", err)
	}
}

func workloadBackend() *cogitodevv1alpha1.LLMBackend {
	return &cogitodevv1alpha1.LLMBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMBackendSpec{
			Type:     cogitodevv1alpha1.BackendVLLM,
			Capacity: &cogitodevv1alpha1.BackendCapacitySpec{GPUs: 2},
			Workload: &cogitodevv1alpha1.BackendWorkloadSpec{
				ContainerName: "runtime",
				Service:       cogitodevv1alpha1.BackendServiceSpec{Port: 8000},
				Deployment: cogitodevv1alpha1.BackendDeploymentSpec{PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runtime", Image: "example.invalid/runtime", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("2")}, Limits: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("2")}}}},
				}}},
			},
		},
	}
}

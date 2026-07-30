package admission

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestLLMModelValidator(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cogitodevv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	existing := validModel("existing", "acme/model")
	backend := &cogitodevv1alpha1.LLMBackend{}
	backend.Name, backend.Namespace = "vllm", "llm"
	backend.Spec.Type = cogitodevv1alpha1.BackendVLLM
	validator := &LLMModelValidator{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing, backend).Build()}

	duplicate := validModel("duplicate", "acme/model")
	if _, err := validator.ValidateCreate(context.Background(), duplicate); err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("duplicate error = %v", err)
	}

	missing := validModel("missing", "acme/missing")
	missing.Spec.BackendRef = &corev1.LocalObjectReference{Name: "missing"}
	if _, err := validator.ValidateCreate(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing backend error = %v", err)
	}
}

func validModel(name, canonical string) *cogitodevv1alpha1.LLMModel {
	return &cogitodevv1alpha1.LLMModel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMModelSpec{
			Model:   cogitodevv1alpha1.LLMModelRef{Name: canonical, Source: canonical},
			Serving: cogitodevv1alpha1.ServingSpec{Backend: cogitodevv1alpha1.BackendVLLM, DisplayName: canonical, MaxModelLen: 1, Args: []string{"--host", "0.0.0.0"}},
		},
	}
}

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestModelControllerRejectsUnsupportedDriverCapability(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	backend := backendFor("llama", "llama", "llama", cogitodevv1alpha1.BackendLlamaCpp)
	model := modelFor("model", "acme/model", cogitodevv1alpha1.BackendLlamaCpp)
	model.Finalizers = []string{cogitodevv1alpha1.ModelProtectionFinalizer}
	model.Spec.Serving.ToolCallParser = "hermes"
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(model, backend).
		WithObjects(model, backend).
		Build()
	reconciler := &LLMModelReconciler{Client: kubeClient, Scheme: scheme}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var got cogitodevv1alpha1.LLMModel
	if err := kubeClient.Get(context.Background(), request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != cogitodevv1alpha1.ModelPhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if !modelConditionHasReason(got.Status.Conditions, "InvalidServingConfiguration") {
		t.Fatalf("conditions = %#v, want InvalidServingConfiguration", got.Status.Conditions)
	}
}

func TestModelControllerInitialReconcileAddsFinalizerAndStatus(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	backend := backendFor("vllm", "llm-vllm", "llm-vllm", cogitodevv1alpha1.BackendVLLM)
	model := modelFor("model", "acme/model", cogitodevv1alpha1.BackendVLLM)
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(model, backend).
		WithObjects(model, backend).
		Build()
	reconciler := &LLMModelReconciler{Client: kubeClient, Scheme: scheme}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	var got cogitodevv1alpha1.LLMModel
	if err := kubeClient.Get(context.Background(), request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if !containsString(got.Finalizers, cogitodevv1alpha1.ModelProtectionFinalizer) {
		t.Fatalf("finalizers = %v, missing protection finalizer", got.Finalizers)
	}
	if got.Status.Phase != cogitodevv1alpha1.ModelPhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if !modelConditionHasReason(got.Status.Conditions, "Configured") {
		t.Fatalf("conditions = %#v, want Configured", got.Status.Conditions)
	}
}

func modelConditionHasReason(conditions []metav1.Condition, reason string) bool {
	for _, condition := range conditions {
		if condition.Reason == reason {
			return true
		}
	}
	return false
}

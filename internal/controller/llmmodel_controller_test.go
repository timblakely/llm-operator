package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

func TestModelControllerValidatesPinnedChatTemplate(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	backend := backendFor("vllm", "llm-vllm", "llm-vllm", cogitodevv1alpha1.BackendVLLM)
	model := modelFor("model", "acme/model", cogitodevv1alpha1.BackendVLLM)
	model.Finalizers = []string{cogitodevv1alpha1.ModelProtectionFinalizer}
	content := "template"
	digest := sha256.Sum256([]byte(content))
	model.Spec.Serving.ChatTemplate = &cogitodevv1alpha1.ChatTemplateSpec{
		ConfigMapKeyRef: corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "template"}, Key: "chat_template.jinja"},
		SHA256:          hex.EncodeToString(digest[:]),
	}
	template := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "llm"}, Data: map[string]string{"chat_template.jinja": content}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(model, backend).WithObjects(model, backend, template).Build()
	reconciler := &LLMModelReconciler{Client: kubeClient, Scheme: scheme}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var got cogitodevv1alpha1.LLMModel
	if err := kubeClient.Get(context.Background(), request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != cogitodevv1alpha1.ModelPhaseReady || !modelConditionHasReason(got.Status.Conditions, "Resolved") {
		t.Fatalf("status = %#v, want ready Resolved", got.Status)
	}
}

func TestModelControllerRejectsMismatchedChatTemplate(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	backend := backendFor("vllm", "llm-vllm", "llm-vllm", cogitodevv1alpha1.BackendVLLM)
	model := modelFor("model", "acme/model", cogitodevv1alpha1.BackendVLLM)
	model.Finalizers = []string{cogitodevv1alpha1.ModelProtectionFinalizer}
	model.Spec.Serving.ChatTemplate = &cogitodevv1alpha1.ChatTemplateSpec{
		ConfigMapKeyRef: corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "template"}, Key: "chat_template.jinja"},
		SHA256:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	template := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "llm"}, Data: map[string]string{"chat_template.jinja": "different"}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(model, backend).WithObjects(model, backend, template).Build()
	reconciler := &LLMModelReconciler{Client: kubeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: model.Name, Namespace: model.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var got cogitodevv1alpha1.LLMModel
	if err := kubeClient.Get(context.Background(), request.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != cogitodevv1alpha1.ModelPhaseFailed || !modelConditionHasReason(got.Status.Conditions, "TemplateInvalid") {
		t.Fatalf("status = %#v, want failed TemplateInvalid", got.Status)
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

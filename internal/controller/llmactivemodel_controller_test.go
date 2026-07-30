package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestReconcileWithTransitionsDisabledDoesNotMutateDeployment(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	deployment := testDeployment()
	model := testModel()
	backend := testBackend()
	active := &cogitodevv1alpha1.LLMActiveModel{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "llm"},
		Spec:       cogitodevv1alpha1.LLMActiveModelSpec{ModelName: model.Spec.Model.Name},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(active, model, backend).WithObjects(deployment, model, backend, active).Build()
	reconciler := &LLMActiveModelReconciler{Client: client, Scheme: scheme, TransitionsEnabled: false}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: active.Name, Namespace: active.Namespace}}); err != nil {
		t.Fatal(err)
	}

	var gotDeployment appsv1.Deployment
	if err := client.Get(context.Background(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &gotDeployment); err != nil {
		t.Fatal(err)
	}
	if gotDeployment.Spec.Replicas == nil || *gotDeployment.Spec.Replicas != 0 {
		t.Fatalf("disabled transition changed replicas to %v", gotDeployment.Spec.Replicas)
	}
	if gotDeployment.Spec.Template.Spec.Containers[0].Args[0] != "--old-model" {
		t.Fatalf("disabled transition changed backend args: %q", gotDeployment.Spec.Template.Spec.Containers[0].Args)
	}

	var gotActive cogitodevv1alpha1.LLMActiveModel
	if err := client.Get(context.Background(), types.NamespacedName{Name: active.Name, Namespace: active.Namespace}, &gotActive); err != nil {
		t.Fatal(err)
	}
	if gotActive.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseTransitioning {
		t.Fatalf("phase = %q, want Transitioning", gotActive.Status.Phase)
	}
	if !hasActiveCondition(gotActive.Status.Conditions, "TransitionsDisabled") {
		t.Fatalf("missing TransitionsDisabled condition: %#v", gotActive.Status.Conditions)
	}
}

func TestActivateDeploymentPreservesSidecars(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	deployment := testDeployment()
	model := testModel()
	backend := testBackend()
	active := &cogitodevv1alpha1.LLMActiveModel{ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "llm"}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
	reconciler := &LLMActiveModelReconciler{Client: client, Scheme: scheme}

	if err := reconciler.activateDeployment(context.Background(), active, model, backend); err != nil {
		t.Fatal(err)
	}

	var got appsv1.Deployment
	if err := client.Get(context.Background(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", got.Spec.Replicas)
	}
	if len(got.Spec.Template.Spec.Containers) != 2 || got.Spec.Template.Spec.Containers[1].Name != "proxy" {
		t.Fatalf("sidecar was not preserved: %#v", got.Spec.Template.Spec.Containers)
	}
	if got.Spec.Template.Spec.Containers[1].Args[0] != "--proxy-config" {
		t.Fatalf("sidecar args changed: %q", got.Spec.Template.Spec.Containers[1].Args)
	}
	if got.Spec.Template.Spec.Containers[0].Args[len(got.Spec.Template.Spec.Containers[0].Args)-2] != "--served-model-name" {
		t.Fatalf("backend args were not activated: %q", got.Spec.Template.Spec.Containers[0].Args)
	}
}

func TestActivateDeploymentIsIdempotentAfterActivation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	deployment := testDeployment()
	model := testModel()
	backend := testBackend()
	active := &cogitodevv1alpha1.LLMActiveModel{ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "llm"}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
	reconciler := &LLMActiveModelReconciler{Client: client, Scheme: scheme}

	if err := reconciler.activateDeployment(context.Background(), active, model, backend); err != nil {
		t.Fatal(err)
	}
	var first appsv1.Deployment
	if err := client.Get(context.Background(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &first); err != nil {
		t.Fatal(err)
	}
	switchedAt := first.Spec.Template.Annotations[switchedAtAnno]
	if err := reconciler.activateDeployment(context.Background(), active, model, backend); err != nil {
		t.Fatal(err)
	}
	var second appsv1.Deployment
	if err := client.Get(context.Background(), types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, &second); err != nil {
		t.Fatal(err)
	}
	if got := second.Spec.Template.Annotations[switchedAtAnno]; got != switchedAt {
		t.Fatalf("repeated activation changed switched-at from %q to %q", switchedAt, got)
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cogitodevv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testModel() *cogitodevv1alpha1.LLMModel {
	return &cogitodevv1alpha1.LLMModel{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMModelSpec{
			Model:   cogitodevv1alpha1.LLMModelRef{Name: "acme/model", Source: "acme/model"},
			Serving: cogitodevv1alpha1.ServingSpec{Backend: cogitodevv1alpha1.BackendVLLM, DisplayName: "Model", MaxModelLen: 8192},
		},
	}
}

func testBackend() *cogitodevv1alpha1.LLMBackend {
	return &cogitodevv1alpha1.LLMBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "vllm", Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMBackendSpec{
			Type:          cogitodevv1alpha1.BackendVLLM,
			DeploymentRef: corev1.LocalObjectReference{Name: "backend"},
			ContainerName: "vllm",
			ServiceRef:    corev1.LocalObjectReference{Name: "backend"},
			Port:          8000,
		},
	}
}

func testDeployment() *appsv1.Deployment {
	zero := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "llm"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &zero,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "vllm", Args: []string{"--old-model"}},
				{Name: "proxy", Args: []string{"--proxy-config"}},
			}}},
		},
	}
}

func hasActiveCondition(conditions []metav1.Condition, reason string) bool {
	for _, condition := range conditions {
		if condition.Reason == reason {
			return true
		}
	}
	return false
}

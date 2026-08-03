package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestLLMBackendWorkloadCreatesOwnedObjectsAndPreservesTransitionReplicas(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	zero := int32(0)
	backend := &cogitodevv1alpha1.LLMBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "owned-vllm", Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMBackendSpec{
			Type: cogitodevv1alpha1.BackendVLLM,
			Workload: &cogitodevv1alpha1.BackendWorkloadSpec{
				ContainerName: "runtime",
				Service:       cogitodevv1alpha1.BackendServiceSpec{Name: "owned-vllm-service", Port: 8000},
				Deployment: cogitodevv1alpha1.BackendDeploymentSpec{
					Name:     "owned-vllm-deployment",
					Replicas: &zero,
					Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
					PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  "runtime",
						Image: "example.invalid/vllm@sha256:deadbeef",
					}}}},
				},
			},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(backend).WithObjects(backend).Build()
	reconciler := &LLMBackendReconciler{Client: client, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: backend.Name, Namespace: backend.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	// The first pass adds the deletion-protection finalizer. The next pass
	// creates the children, matching controller-runtime's normal requeue.
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	var service corev1.Service
	if err := client.Get(context.Background(), types.NamespacedName{Name: "owned-vllm-service", Namespace: backend.Namespace}, &service); err != nil {
		t.Fatalf("generated service: %v", err)
	}
	if service.Spec.Selector[backendLabel] != backend.Name || service.Spec.Ports[0].Port != 8000 {
		t.Fatalf("generated service = %#v", service.Spec)
	}
	if len(service.OwnerReferences) != 1 || service.OwnerReferences[0].Name != backend.Name {
		t.Fatalf("service owner references = %#v", service.OwnerReferences)
	}

	var deployment appsv1.Deployment
	if err := client.Get(context.Background(), types.NamespacedName{Name: "owned-vllm-deployment", Namespace: backend.Namespace}, &deployment); err != nil {
		t.Fatalf("generated deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("initial replicas = %v, want zero", deployment.Spec.Replicas)
	}
	if deployment.Spec.Template.Labels[backendLabel] != backend.Name || deployment.Spec.Template.Spec.Containers[0].Name != "runtime" {
		t.Fatalf("generated deployment template = %#v", deployment.Spec.Template)
	}

	one := int32(1)
	deployment.Spec.Replicas = &one
	if err := client.Update(context.Background(), &deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Name: "owned-vllm-deployment", Namespace: backend.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("backend reconcile reset transition replicas to %v", deployment.Spec.Replicas)
	}
}

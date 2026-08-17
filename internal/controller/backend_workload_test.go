package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	deployment.Spec.Template.Annotations = map[string]string{
		activeModelAnno:  "Lorbus/Qwen3.6-27B-int4-AutoRound",
		chatTemplateAnno: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	deployment.Spec.Template.Spec.Containers[0].Args = []string{"--chat-template", chatTemplateMountDir + "/" + chatTemplateMountFile}
	deployment.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      chatTemplateVolumeName,
		MountPath: chatTemplateMountDir,
		ReadOnly:  true,
	}}
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: chatTemplateVolumeName,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "qwen-template"},
			Items:                []corev1.KeyToPath{{Key: "chat_template.jinja", Path: chatTemplateMountFile}},
		}},
	}}
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
	if got := deployment.Spec.Template.Spec.Containers[0].Args; len(got) != 2 || got[1] != chatTemplateMountDir+"/"+chatTemplateMountFile {
		t.Fatalf("backend reconcile dropped injected runtime args: %#v", got)
	}
	if len(deployment.Spec.Template.Spec.Volumes) != 1 || deployment.Spec.Template.Spec.Volumes[0].Name != chatTemplateVolumeName {
		t.Fatalf("backend reconcile dropped injected chat-template volume: %#v", deployment.Spec.Template.Spec.Volumes)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].VolumeMounts; len(got) != 1 || got[0].Name != chatTemplateVolumeName || got[0].MountPath != chatTemplateMountDir {
		t.Fatalf("backend reconcile dropped injected chat-template mount: %#v", got)
	}
}

// TestLLMBackendWorkloadPatchesRatherThanReplacesAConcurrentlyModifiedDeployment
// proves ensureBackendWorkload writes the generated Deployment with a merge
// Patch, not a full Update: a full Update carries the resourceVersion read at
// Get time and is rejected outright by a concurrent writer (this is exactly
// the "the object has been modified" conflict LLMActiveModel's own patches
// were producing against this reconciler in production), and even a
// successful full Update would silently discard a field this reconciler
// never touches. A merge patch does neither.
func TestLLMBackendWorkloadPatchesRatherThanReplacesAConcurrentlyModifiedDeployment(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	one := int32(1)
	backend := &cogitodevv1alpha1.LLMBackend{
		ObjectMeta: metav1.ObjectMeta{Name: "owned-vllm", Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMBackendSpec{
			Type: cogitodevv1alpha1.BackendVLLM,
			Workload: &cogitodevv1alpha1.BackendWorkloadSpec{
				ContainerName: "runtime",
				Service:       cogitodevv1alpha1.BackendServiceSpec{Name: "owned-vllm-service", Port: 8000},
				Deployment: cogitodevv1alpha1.BackendDeploymentSpec{
					Name:     "owned-vllm-deployment",
					Replicas: &one,
					PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name:  "runtime",
						Image: "example.invalid/vllm@sha256:deadbeef",
					}}}},
				},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(backend).WithObjects(backend).Build()
	reconciler := &LLMBackendReconciler{Client: fakeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: backend.Name, Namespace: backend.Namespace}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	// Change the backend spec so the next reconcile has a real diff to write,
	// not a no-op.
	var current cogitodevv1alpha1.LLMBackend
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: backend.Name, Namespace: backend.Namespace}, &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Workload.Deployment.PodTemplate.Spec.Containers[0].Image = "example.invalid/vllm@sha256:cafebabe"
	if err := fakeClient.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}

	raced := false
	intercepted := interceptor.NewClient(fakeClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			deployment, ok := obj.(*appsv1.Deployment)
			if !ok || raced || deployment.Name != "owned-vllm-deployment" {
				return nil
			}
			raced = true
			// Simulate LLMActiveModel concurrently patching this same
			// Deployment's runtime args mid-reconcile, exactly the race this
			// fix targets: a second, independent writer changes the object
			// between ensureBackendWorkload's Get and its write.
			var concurrent appsv1.Deployment
			if err := c.Get(ctx, key, &concurrent); err != nil {
				return err
			}
			if concurrent.Annotations == nil {
				concurrent.Annotations = map[string]string{}
			}
			concurrent.Annotations[activeModelAnno] = "concurrently-set-model"
			return c.Update(ctx, &concurrent)
		},
	})

	racedReconciler := &LLMBackendReconciler{Client: intercepted, Scheme: scheme}
	if _, err := racedReconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile against a concurrently-modified Deployment = %v, want no conflict (a merge patch must not require an unchanged resourceVersion)", err)
	}

	var deployment appsv1.Deployment
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "owned-vllm-deployment", Namespace: backend.Namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Annotations[activeModelAnno] != "concurrently-set-model" {
		t.Fatalf("activeModelAnno = %q, want the concurrent writer's value to survive a merge patch instead of being clobbered by a full Update", deployment.Annotations[activeModelAnno])
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "example.invalid/vllm@sha256:cafebabe" {
		t.Fatalf("image = %q, want the reconciler's own change to still apply alongside the concurrent write", got)
	}
}

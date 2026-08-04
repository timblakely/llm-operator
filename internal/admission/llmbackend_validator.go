package admission

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	admission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

// LLMBackendValidator enforces the ownership boundary between a backend's
// workload baseline and the model arguments injected by the transition
// controller.
type LLMBackendValidator struct{}

func (LLMBackendValidator) ValidateCreate(_ context.Context, backend *cogitodevv1alpha1.LLMBackend) (admission.Warnings, error) {
	return nil, validateBackend(backend)
}

func (LLMBackendValidator) ValidateUpdate(_ context.Context, _ *cogitodevv1alpha1.LLMBackend, backend *cogitodevv1alpha1.LLMBackend) (admission.Warnings, error) {
	return nil, validateBackend(backend)
}

func (LLMBackendValidator) ValidateDelete(context.Context, *cogitodevv1alpha1.LLMBackend) (admission.Warnings, error) {
	return nil, nil
}

func validateBackend(backend *cogitodevv1alpha1.LLMBackend) error {
	hasReferences := backend.Spec.DeploymentRef.Name != "" || backend.Spec.ServiceRef.Name != "" || backend.Spec.ContainerName != "" || backend.Spec.Port != 0
	if backend.Spec.Workload == nil {
		if !hasReferences || backend.Spec.DeploymentRef.Name == "" || backend.Spec.ServiceRef.Name == "" || backend.Spec.ContainerName == "" || backend.Spec.Port == 0 {
			return fmt.Errorf("backend must declare either workload or complete deprecated deployment/service references")
		}
		return nil
	}
	if hasReferences {
		return fmt.Errorf("workload mode cannot be combined with deprecated deploymentRef, serviceRef, containerName, or port")
	}
	workload := backend.Spec.Workload
	if backend.Spec.Capacity == nil || backend.Spec.Capacity.GPUs < 1 {
		return fmt.Errorf("workload mode requires capacity.gpus")
	}
	if workload.ContainerName == "" || workload.Service.Port == 0 {
		return fmt.Errorf("workload.containerName and workload.service.port are required")
	}
	count := 0
	for _, container := range workload.Deployment.PodTemplate.Spec.Containers {
		if container.Name == workload.ContainerName {
			count++
			if len(container.Args) != 0 {
				return fmt.Errorf("runtime container %q must not set args; LLMModel supplies model-specific runtime arguments", container.Name)
			}
			resourceName := corev1.ResourceName(backend.Spec.Capacity.ResourceName)
			if resourceName == "" {
				resourceName = corev1.ResourceName("nvidia.com/gpu")
			}
			want := backend.Spec.Capacity.GPUs
			request := container.Resources.Requests[resourceName]
			limit := container.Resources.Limits[resourceName]
			if request.Value() != int64(want) || limit.Value() != int64(want) {
				return fmt.Errorf("runtime container %q GPU request and limit for %q must both equal capacity.gpus=%d", container.Name, resourceName, want)
			}
		}
	}
	if count != 1 {
		return fmt.Errorf("workload.containerName %q must select exactly one container", workload.ContainerName)
	}
	if value := workload.Deployment.PodTemplate.Labels["llm.cogito.dev/backend"]; value != "" && value != backend.Name {
		return fmt.Errorf("pod template label llm.cogito.dev/backend is reserved for the controller")
	}
	return nil
}

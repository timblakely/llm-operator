/*
 * Copyright 2025.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

package controller

import (
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

const backendLabel = "llm.cogito.dev/backend"

func backendDeploymentName(backend *cogitodevv1alpha1.LLMBackend) string {
	if backend.Spec.Workload != nil {
		if name := backend.Spec.Workload.Deployment.Name; name != "" {
			return name
		}
		return backend.Name
	}
	return backend.Spec.DeploymentRef.Name
}

func backendServiceName(backend *cogitodevv1alpha1.LLMBackend) string {
	if backend.Spec.Workload != nil {
		if name := backend.Spec.Workload.Service.Name; name != "" {
			return name
		}
		return backend.Name
	}
	return backend.Spec.ServiceRef.Name
}

func backendContainerName(backend *cogitodevv1alpha1.LLMBackend) string {
	if backend.Spec.Workload != nil {
		return backend.Spec.Workload.ContainerName
	}
	return backend.Spec.ContainerName
}

func backendPort(backend *cogitodevv1alpha1.LLMBackend) int {
	if backend.Spec.Workload != nil {
		return backend.Spec.Workload.Service.Port
	}
	return backend.Spec.Port
}

func backendLabels(backend *cogitodevv1alpha1.LLMBackend) map[string]string {
	return map[string]string{backendLabel: backend.Name}
}

func mergeLabels(existing, required map[string]string) map[string]string {
	out := make(map[string]string, len(existing)+len(required))
	for key, value := range existing {
		out[key] = value
	}
	for key, value := range required {
		out[key] = value
	}
	return out
}

// ensureBackendWorkload materializes a workload-mode backend. The user owns
// the native pod template; the controller owns names, selectors, and labels.
func (r *LLMBackendReconciler) ensureBackendWorkload(ctx context.Context, backend *cogitodevv1alpha1.LLMBackend) error {
	if backend.Spec.Workload == nil {
		return nil
	}
	workload := backend.Spec.Workload
	if workload.ContainerName == "" || workload.Service.Port == 0 {
		return fmt.Errorf("workload.containerName and workload.service.port are required")
	}
	if !hasContainer(workload.Deployment.PodTemplate.Spec.Containers, workload.ContainerName) {
		return fmt.Errorf("workload.containerName %q is not present in workload.deployment.podTemplate", workload.ContainerName)
	}

	labels := backendLabels(backend)
	var service corev1.Service
	serviceKey := client.ObjectKey{Namespace: backend.Namespace, Name: backendServiceName(backend)}
	serviceExists := true
	if err := r.Get(ctx, serviceKey, &service); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get generated service: %w", err)
		}
		serviceExists = false
		service = corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: serviceKey.Name, Namespace: serviceKey.Namespace}}
	}
	serviceBefore := service.DeepCopy()
	service.Labels = mergeLabels(service.Labels, labels)
	service.Annotations = mergeLabels(service.Annotations, workload.Service.Annotations)
	service.Spec = corev1.ServiceSpec{
		Type:           corev1.ServiceTypeClusterIP,
		ClusterIP:      service.Spec.ClusterIP,
		ClusterIPs:     service.Spec.ClusterIPs,
		IPFamilies:     service.Spec.IPFamilies,
		IPFamilyPolicy: service.Spec.IPFamilyPolicy,
		Selector:       labels,
		Ports: []corev1.ServicePort{{
			Name:       workload.Service.PortName,
			Port:       int32(workload.Service.Port),
			TargetPort: intstr.FromInt(workload.Service.Port),
			Protocol:   corev1.ProtocolTCP,
		}},
	}
	if service.Spec.Ports[0].Name == "" {
		service.Spec.Ports[0].Name = "http"
	}
	if err := controllerutil.SetControllerReference(backend, &service, r.Scheme); err != nil {
		return fmt.Errorf("set service owner: %w", err)
	}
	if !serviceExists {
		if err := r.Create(ctx, &service); err != nil {
			return fmt.Errorf("create generated service: %w", err)
		}
	} else if !reflect.DeepEqual(serviceBefore, &service) {
		// A merge patch, not a full Update, so a concurrent write to a field
		// this reconcile never touches (for example LLMActiveModel patching
		// the Deployment's runtime args) cannot land in the read-modify-write
		// gap between this reconcile's Get and its write and get discarded by
		// an unrelated full-object overwrite here.
		if err := r.Patch(ctx, &service, client.MergeFrom(serviceBefore)); err != nil {
			return fmt.Errorf("update generated service: %w", err)
		}
	}

	podTemplate := workload.Deployment.PodTemplate.DeepCopy()
	podTemplate.Labels = mergeLabels(podTemplate.Labels, labels)
	replicas := ptr.Deref(workload.Deployment.Replicas, int32(0))
	var deployment appsv1.Deployment
	deploymentKey := client.ObjectKey{Namespace: backend.Namespace, Name: backendDeploymentName(backend)}
	deploymentExists := true
	if err := r.Get(ctx, deploymentKey, &deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get generated deployment: %w", err)
		}
		deploymentExists = false
		deployment = appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentKey.Name, Namespace: deploymentKey.Namespace}}
	}
	deploymentBefore := deployment.DeepCopy()
	currentReplicas := deployment.Spec.Replicas
	// Runtime arguments and transition annotations are owned by
	// LLMActiveModel. A normal backend reconcile must not revert a generated
	// workload to its intentionally argument-free template while it is active.
	if deploymentExists {
		preserveTransitionRuntime(&deployment.Spec.Template, podTemplate, workload.ContainerName)
	}
	deployment.Labels = mergeLabels(deployment.Labels, labels)
	deployment.Spec = appsv1.DeploymentSpec{
		Replicas: ptr.To(replicas),
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: *podTemplate,
		Strategy: workload.Deployment.Strategy,
	}
	if err := controllerutil.SetControllerReference(backend, &deployment, r.Scheme); err != nil {
		return fmt.Errorf("set deployment owner: %w", err)
	}
	if deploymentExists {
		// Replica count is transition state, not baseline configuration. Never
		// reset an active backend to zero during an ordinary backend reconcile.
		deployment.Spec.Replicas = currentReplicas
	}
	if !deploymentExists {
		if err := r.Create(ctx, &deployment); err != nil {
			return fmt.Errorf("create generated deployment: %w", err)
		}
	} else if !reflect.DeepEqual(deploymentBefore, &deployment) {
		// See the Service patch above: a merge patch avoids racing
		// LLMActiveModel's own concurrent Patch of this same Deployment
		// during a transition, which a full Update here could silently
		// overwrite with a stale read.
		if err := r.Patch(ctx, &deployment, client.MergeFrom(deploymentBefore)); err != nil {
			return fmt.Errorf("update generated deployment: %w", err)
		}
	}
	return nil
}

func preserveTransitionRuntime(current *corev1.PodTemplateSpec, desired *corev1.PodTemplateSpec, containerName string) {
	var currentRuntime *corev1.Container
	for i := range desired.Spec.Containers {
		if desired.Spec.Containers[i].Name != containerName {
			continue
		}
		for j := range current.Spec.Containers {
			if current.Spec.Containers[j].Name == containerName {
				desired.Spec.Containers[i].Args = append([]string(nil), current.Spec.Containers[j].Args...)
				currentRuntime = &current.Spec.Containers[j]
				break
			}
		}
		break
	}
	// The active-model controller owns this reserved ConfigMap volume/mount
	// pair while a model supplies a chat template. Preserve it with the
	// injected runtime arguments so an ordinary backend reconciliation cannot
	// leave --chat-template pointing at a nonexistent file.
	if currentRuntime != nil {
		preserveChatTemplateMount(current, desired, currentRuntime, containerName)
	}
	if current.Annotations == nil {
		return
	}
	if desired.Annotations == nil {
		desired.Annotations = make(map[string]string)
	}
	for _, key := range []string{activeModelAnno, switchedAtAnno, chatTemplateAnno} {
		if value, ok := current.Annotations[key]; ok {
			desired.Annotations[key] = value
		}
	}
}

func preserveChatTemplateMount(current *corev1.PodTemplateSpec, desired *corev1.PodTemplateSpec, currentRuntime *corev1.Container, containerName string) {
	var currentVolume *corev1.Volume
	for i := range current.Spec.Volumes {
		if current.Spec.Volumes[i].Name == chatTemplateVolumeName {
			currentVolume = &current.Spec.Volumes[i]
			break
		}
	}
	var currentMount *corev1.VolumeMount
	for i := range currentRuntime.VolumeMounts {
		if currentRuntime.VolumeMounts[i].Name == chatTemplateVolumeName {
			currentMount = &currentRuntime.VolumeMounts[i]
			break
		}
	}
	if currentVolume == nil || currentMount == nil {
		return
	}

	volumes := desired.Spec.Volumes[:0]
	for _, volume := range desired.Spec.Volumes {
		if volume.Name != chatTemplateVolumeName {
			volumes = append(volumes, volume)
		}
	}
	desired.Spec.Volumes = append(volumes, *currentVolume.DeepCopy())
	for i := range desired.Spec.Containers {
		if desired.Spec.Containers[i].Name != containerName {
			continue
		}
		mounts := desired.Spec.Containers[i].VolumeMounts[:0]
		for _, mount := range desired.Spec.Containers[i].VolumeMounts {
			if mount.Name != chatTemplateVolumeName {
				mounts = append(mounts, mount)
			}
		}
		desired.Spec.Containers[i].VolumeMounts = append(mounts, *currentMount.DeepCopy())
		return
	}
}

func hasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

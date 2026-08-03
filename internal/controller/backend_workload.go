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
		if err := r.Update(ctx, &service); err != nil {
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
		if err := r.Update(ctx, &deployment); err != nil {
			return fmt.Errorf("update generated deployment: %w", err)
		}
	}
	return nil
}

func hasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

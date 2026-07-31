/*
 * Copyright 2025.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
	runtimebackend "github.com/timblakely/llm-operator/internal/backend"
)

const (
	DeploymentExistsCondition = "DeploymentExists"
	ModelLoadedCondition      = "ModelLoaded"
)

// LLMBackendReconciler reconciles a LLMBackend object.
type LLMBackendReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
}

// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmbackends,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmbackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

func (r *LLMBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("llmbackend", req.NamespacedName)

	var backend cogitodevv1alpha1.LLMBackend
	if err := r.Get(ctx, req.NamespacedName, &backend); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	backend.Status.ObservedGeneration = backend.GetGeneration()

	// Look up referenced Deployment
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: backend.Spec.DeploymentRef.Name, Namespace: backend.Namespace}, &deployment); err != nil {
		setBackendCondition(&backend.Status, DeploymentExistsCondition, metav1.ConditionFalse, "NotFound", err.Error())
		backend.Status.Phase = cogitodevv1alpha1.BackendPhaseFailed
		backend.Status.Replicas = 0
		backend.Status.AvailableReplicas = 0
		_ = r.Status().Update(ctx, &backend)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	setBackendCondition(&backend.Status, DeploymentExistsCondition, metav1.ConditionTrue, "Exists", "Deployment exists")
	backend.Status.Replicas = deployment.Status.Replicas
	backend.Status.AvailableReplicas = deployment.Status.AvailableReplicas

	// Read active model from Deployment annotations
	if annotations := deployment.Spec.Template.Annotations; annotations != nil {
		if activeModel, ok := annotations["llm.cogito.dev/active-model"]; ok {
			backend.Status.ActiveModel = activeModel
			if switchedAt, ok := annotations["llm.cogito.dev/switched-at"]; ok {
				if t, err := time.Parse(time.RFC3339Nano, switchedAt); err == nil {
					backend.Status.ActiveModelSince = &metav1.Time{Time: t}
				}
			}
		}
	}

	// Health check. A Deployment can report Available before its Service endpoint
	// accepts connections; retry transient probe failures so status converges
	// without waiting for another Deployment event.
	retryHealth := false
	if r.HTTPClient != nil && backend.Status.AvailableReplicas > 0 {
		driver, driverErr := runtimebackend.DefaultRegistry().Driver(backend.Spec.Type)
		if driverErr != nil {
			setBackendCondition(&backend.Status, BackendHealthyCondition, metav1.ConditionFalse, "UnsupportedBackend", driverErr.Error())
			backend.Status.Phase = cogitodevv1alpha1.BackendPhaseFailed
			if err := r.Status().Update(ctx, &backend); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		backendURL := fmt.Sprintf("http://%s:%d", backend.Spec.ServiceRef.Name, backend.Spec.Port)
		if err := driver.CheckHealth(ctx, r.HTTPClient, backendURL); err == nil {
			setBackendCondition(&backend.Status, BackendHealthyCondition, metav1.ConditionTrue, "Healthy", "Backend is healthy")
			setBackendCondition(&backend.Status, ModelLoadedCondition, metav1.ConditionTrue, "Loaded", "Model is loaded")
			backend.Status.Phase = cogitodevv1alpha1.BackendPhaseServing
		} else {
			setBackendCondition(&backend.Status, BackendHealthyCondition, metav1.ConditionFalse, "Unhealthy", err.Error())
			backend.Status.Phase = cogitodevv1alpha1.BackendPhaseStarting
			retryHealth = true
		}
	} else if backend.Status.AvailableReplicas == 0 {
		setBackendCondition(&backend.Status, BackendHealthyCondition, metav1.ConditionFalse, "NoReplicas", "No available replicas")
		backend.Status.Phase = cogitodevv1alpha1.BackendPhaseStopped
	} else {
		backend.Status.Phase = cogitodevv1alpha1.BackendPhaseStarting
	}

	if err := r.Status().Update(ctx, &backend); err != nil {
		logger.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	if retryHealth {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func setBackendCondition(status *cogitodevv1alpha1.LLMBackendStatus, conditionType string, statusVal metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range status.Conditions {
		if c.Type == conditionType {
			if c.Status != statusVal || c.Reason != reason || c.Message != message {
				status.Conditions[i].Status = statusVal
				status.Conditions[i].Reason = reason
				status.Conditions[i].Message = message
				status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             statusVal,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

func (r *LLMBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.HTTPClient == nil {
		r.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cogitodevv1alpha1.LLMBackend{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			var list cogitodevv1alpha1.LLMBackendList
			if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			var requests []ctrl.Request
			for _, b := range list.Items {
				if b.Spec.DeploymentRef.Name == obj.GetName() {
					requests = append(requests, ctrl.Request{
						NamespacedName: client.ObjectKeyFromObject(&b),
					})
				}
			}
			return requests
		})).
		Complete(r)
}

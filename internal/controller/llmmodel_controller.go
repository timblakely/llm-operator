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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	logr "github.com/go-logr/logr"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
	"github.com/timblakely/llm-operator/internal/backend"
)

const (
	ModelConfiguredCondition = "ModelConfigured"
	ArtifactCachedCondition  = "ArtifactCached"
	BackendHealthyCondition  = "BackendHealthy"
)

// LLMModelReconciler reconciles a LLMModel object.
type LLMModelReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodels/finalizers,verbs=update
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmbackends,verbs=get;list;watch

func (r *LLMModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("llmmodel", req.NamespacedName)

	var model cogitodevv1alpha1.LLMModel
	if err := r.Get(ctx, req.NamespacedName, &model); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !model.DeletionTimestamp.IsZero() {
		if model.Status.Active {
			logger.Info("cannot delete active model, deactivate first")
			return ctrl.Result{}, fmt.Errorf("cannot delete active model %s, deactivate first", model.Spec.Model.Name)
		}
		controllerutil.RemoveFinalizer(&model, cogitodevv1alpha1.ModelProtectionFinalizer)
		if err := r.Update(ctx, &model); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("finalizer removed, model will be deleted")
		return ctrl.Result{}, nil
	}

	// Ensure finalizer
	if !containsString(model.Finalizers, cogitodevv1alpha1.ModelProtectionFinalizer) {
		controllerutil.AddFinalizer(&model, cogitodevv1alpha1.ModelProtectionFinalizer)
		if err := r.Update(ctx, &model); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Reconcile model
	return r.reconcileModel(ctx, &model, logger)
}

func (r *LLMModelReconciler) reconcileModel(ctx context.Context, model *cogitodevv1alpha1.LLMModel, logger logr.Logger) (ctrl.Result, error) {
	model.Status.ObservedGeneration = model.GetGeneration()

	// Find the backend
	_, err := r.resolveBackend(ctx, model)
	if err != nil {
		setCondition(&model.Status, ModelConfiguredCondition, metav1.ConditionFalse, "BackendNotFound", err.Error())
		model.Status.Phase = cogitodevv1alpha1.ModelPhaseFailed
		if updateErr := r.Status().Update(ctx, model); updateErr != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", updateErr)
		}
		return ctrl.Result{}, nil
	}

	// Validate args don't contain injected flags
	if err := validateArgs(model); err != nil {
		setCondition(&model.Status, ModelConfiguredCondition, metav1.ConditionFalse, "InvalidServingConfiguration", err.Error())
		model.Status.Phase = cogitodevv1alpha1.ModelPhaseFailed
		if updateErr := r.Status().Update(ctx, model); updateErr != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", updateErr)
		}
		return ctrl.Result{}, nil
	}

	setCondition(&model.Status, ModelConfiguredCondition, metav1.ConditionTrue, "Configured", "Model is configured and backend exists")

	// Determine phase
	if model.Status.Active {
		model.Status.Phase = cogitodevv1alpha1.ModelPhaseActive
	} else {
		model.Status.Phase = cogitodevv1alpha1.ModelPhaseReady
	}

	if err := r.Status().Update(ctx, model); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *LLMModelReconciler) resolveBackend(ctx context.Context, model *cogitodevv1alpha1.LLMModel) (*cogitodevv1alpha1.LLMBackend, error) {
	if model.Spec.BackendRef != nil {
		var backend cogitodevv1alpha1.LLMBackend
		if err := r.Get(ctx, types.NamespacedName{Name: model.Spec.BackendRef.Name, Namespace: model.Namespace}, &backend); err != nil {
			return nil, fmt.Errorf("backend %q not found: %w", model.Spec.BackendRef.Name, err)
		}
		return &backend, nil
	}

	// Find default backend matching the serving type
	var backendList cogitodevv1alpha1.LLMBackendList
	if err := r.List(ctx, &backendList, client.InNamespace(model.Namespace)); err != nil {
		return nil, fmt.Errorf("listing backends: %w", err)
	}

	for i := range backendList.Items {
		if backendList.Items[i].Spec.Type == model.Spec.Serving.Backend {
			return &backendList.Items[i], nil
		}
	}

	return nil, fmt.Errorf("no backend found for type %q", model.Spec.Serving.Backend)
}

// validateArgs checks that the model args don't contain controller-injected flags.
func validateArgs(model *cogitodevv1alpha1.LLMModel) error {
	driver, err := backend.DefaultRegistry().Driver(model.Spec.Serving.Backend)
	if err != nil {
		return err
	}
	return driver.Validate(model)
}

// effectiveArgs returns the full args list with controller-injected flags.
func effectiveArgs(model *cogitodevv1alpha1.LLMModel) []string {
	driver, err := backend.DefaultRegistry().Driver(model.Spec.Serving.Backend)
	if err != nil {
		return append([]string(nil), model.Spec.Serving.Args...)
	}
	return driver.EffectiveArgs(model)
}

func (r *LLMModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cogitodevv1alpha1.LLMModel{}).
		Watches(&cogitodevv1alpha1.LLMBackend{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			var list cogitodevv1alpha1.LLMModelList
			if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			var requests []reconcile.Request
			for _, m := range list.Items {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: m.Name, Namespace: m.Namespace},
				})
			}
			return requests
		})).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}

// setCondition updates or appends a condition in the status.
func setCondition(status *cogitodevv1alpha1.LLMModelStatus, conditionType string, statusVal metav1.ConditionStatus, reason, message string) {
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

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

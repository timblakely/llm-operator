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
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

const OverlayValidCondition = "OverlayValid"

// LLMModelOverlayReconciler reconciles a LLMModelOverlay object.
type LLMModelOverlayReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodeloverlays,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodeloverlays/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodels,verbs=get;list;watch

func (r *LLMModelOverlayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("llmmodeloverlay", req.NamespacedName)

	var overlay cogitodevv1alpha1.LLMModelOverlay
	if err := r.Get(ctx, req.NamespacedName, &overlay); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	overlay.Status.ObservedGeneration = overlay.GetGeneration()

	// Validate request defaults
	if err := validateRequestDefaults(overlay.Spec.RequestDefaults); err != nil {
		setOverlayCondition(&overlay.Status, OverlayValidCondition, metav1.ConditionFalse, "InvalidRequestDefaults", err.Error())
		if err := r.Status().Update(ctx, &overlay); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Validate base model exists
	var modelList cogitodevv1alpha1.LLMModelList
	if err := r.List(ctx, &modelList, client.InNamespace(overlay.Namespace)); err != nil {
		logger.Error(err, "failed to list LLMModels")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	found := false
	for _, m := range modelList.Items {
		if m.Spec.Model.Name == overlay.Spec.BaseModel {
			found = true
			overlay.Status.ResolvedBaseModel = m.Spec.Model.Name
			break
		}
	}

	if !found {
		setOverlayCondition(&overlay.Status, OverlayValidCondition, metav1.ConditionFalse, "BaseModelNotFound", fmt.Sprintf("no LLMModel with name %q found", overlay.Spec.BaseModel))
		overlay.Status.ResolvedBaseModel = ""
		if err := r.Status().Update(ctx, &overlay); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	setOverlayCondition(&overlay.Status, OverlayValidCondition, metav1.ConditionTrue, "Valid", "Overlay is valid")

	if err := r.Status().Update(ctx, &overlay); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func validateRequestDefaults(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("requestDefaults is not valid JSON: %w", err)
	}

	if _, ok := m["model"]; ok {
		return fmt.Errorf("requestDefaults must not contain a 'model' key")
	}

	return nil
}

func setOverlayCondition(status *cogitodevv1alpha1.LLMModelOverlayStatus, conditionType string, statusVal metav1.ConditionStatus, reason, message string) {
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

func (r *LLMModelOverlayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cogitodevv1alpha1.LLMModelOverlay{}).
		Watches(&cogitodevv1alpha1.LLMModel{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			var list cogitodevv1alpha1.LLMModelOverlayList
			if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
				return nil
			}
			var requests []ctrl.Request
			for _, o := range list.Items {
				requests = append(requests, ctrl.Request{
					NamespacedName: client.ObjectKeyFromObject(&o),
				})
			}
			return requests
		})).
		Complete(r)
}

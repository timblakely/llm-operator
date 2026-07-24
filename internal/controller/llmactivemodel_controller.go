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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	logr "github.com/go-logr/logr"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
	"github.com/timblakely/llm-operator/internal/cache"
)

const (
	activeModelAnno          = "llm.cogito.dev/active-model"
	switchedAtAnno           = "llm.cogito.dev/switched-at"
	backendProbeWait         = 2 * time.Second
	defaultTransitionTimeout = 30 * time.Minute

	ModelActiveCondition   = "ModelActive"
	TransitionCompleteCond = "TransitionComplete"
)

// LLMActiveModelReconciler reconciles a LLMActiveModel object.
type LLMActiveModelReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Recorder          record.EventRecorder
	CacheManagerURL   string
	TransitionTimeout time.Duration
	HTTPClient        *http.Client
}

// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmactivemodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmactivemodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodels,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llm.cogito.dev,resources=llmbackends,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;patch;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *LLMActiveModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("llmactivemodel", req.NamespacedName)

	var activeModel cogitodevv1alpha1.LLMActiveModel
	if err := r.Get(ctx, req.NamespacedName, &activeModel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	activeModel.Status.ObservedGeneration = activeModel.GetGeneration()

	// If already stable with the same model, no-op
	if activeModel.Status.Phase == cogitodevv1alpha1.ActiveModelPhaseStable &&
		activeModel.Status.ModelName == activeModel.Spec.ModelName {
		return ctrl.Result{}, nil
	}

	// Look up target model
	model, err := r.findModel(ctx, activeModel.Namespace, activeModel.Spec.ModelName)
	if err != nil {
		logger.Error(err, "target model not found")
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "ModelNotFound", err.Error())
		_ = r.Status().Update(ctx, &activeModel)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Look up target backend
	backend, err := r.findBackend(ctx, activeModel.Namespace, model.Spec.Serving.Backend, model.Spec.BackendRef)
	if err != nil {
		logger.Error(err, "target backend not found")
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "BackendNotFound", err.Error())
		_ = r.Status().Update(ctx, &activeModel)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// If already serving the target model on the target backend, mark stable
	if activeModel.Status.ModelName == activeModel.Spec.ModelName &&
		activeModel.Status.BackendType == model.Spec.Serving.Backend {
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseStable
		activeModel.Status.BackendType = model.Spec.Serving.Backend
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionTrue, "Active", "Model is active")
		setActiveCondition(&activeModel.Status, TransitionCompleteCond, metav1.ConditionTrue, "Complete", "Transition complete")
		if err := r.Status().Update(ctx, &activeModel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Execute transition
	return r.executeTransition(ctx, &activeModel, model, backend, logger)
}

func (r *LLMActiveModelReconciler) findModel(ctx context.Context, namespace, modelName string) (*cogitodevv1alpha1.LLMModel, error) {
	var list cogitodevv1alpha1.LLMModelList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Model.Name == modelName {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no LLMModel with name %q found", modelName)
}

func (r *LLMActiveModelReconciler) findBackend(ctx context.Context, namespace string, backendType cogitodevv1alpha1.BackendType, backendRef *corev1.LocalObjectReference) (*cogitodevv1alpha1.LLMBackend, error) {
	if backendRef != nil {
		var backend cogitodevv1alpha1.LLMBackend
		if err := r.Get(ctx, types.NamespacedName{Name: backendRef.Name, Namespace: namespace}, &backend); err != nil {
			return nil, err
		}
		return &backend, nil
	}

	var list cogitodevv1alpha1.LLMBackendList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Type == backendType {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no backend found for type %q", backendType)
}

func (r *LLMActiveModelReconciler) executeTransition(ctx context.Context, activeModel *cogitodevv1alpha1.LLMActiveModel, model *cogitodevv1alpha1.LLMModel, backend *cogitodevv1alpha1.LLMBackend, logger logr.Logger) (ctrl.Result, error) {
	timeout := r.TransitionTimeout
	if timeout == 0 {
		timeout = defaultTransitionTimeout
	}

	transitionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Mark transitioning
	if activeModel.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseTransitioning {
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseTransitioning
		activeModel.Status.TransitionFrom = activeModel.Status.ModelName
		activeModel.Status.TransitionStarted = &metav1.Time{Time: time.Now()}
		activeModel.Status.BackendType = model.Spec.Serving.Backend
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "Transitioning", "Transition in progress")
		if err := r.Status().Update(ctx, activeModel); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Step 1: Scale down current backend if different
	currentBackendType := activeModel.Status.BackendType
	if currentBackendType != "" && currentBackendType != model.Spec.Serving.Backend {
		currentBackend, err := r.findBackend(ctx, activeModel.Namespace, currentBackendType, nil)
		if err == nil && currentBackend != nil {
			if err := r.scaleDeployment(transitionCtx, currentBackend.Spec.DeploymentRef.Name, activeModel.Namespace, 0); err != nil {
				logger.Error(err, "failed to scale down current backend")
				activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
				setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "ScaleDownFailed", err.Error())
				_ = r.Status().Update(ctx, activeModel)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			if err := r.waitForScaleDown(transitionCtx, currentBackend.Spec.DeploymentRef.Name, activeModel.Namespace); err != nil {
				logger.Error(err, "timeout waiting for current backend to scale down")
				return ctrl.Result{RequeueAfter: backendProbeWait}, nil
			}
			logger.Info("current backend scaled down", "backend", currentBackend.Name)
		}
	}

	// Step 2: Ensure cached
	if r.CacheManagerURL != "" && model.Spec.Artifact != nil {
		cacheClient := cache.New(r.CacheManagerURL)
		cacheSpec := cache.CacheSpec{
			Kind:     string(model.Spec.Serving.Backend),
			RepoID:   model.Spec.Model.Source,
			Revision: model.Spec.Model.Revision,
			Files:    model.Spec.Artifact.Files,
		}
		if model.Spec.Artifact.ExpectedSize != "" {
			if size, err := parseSize(model.Spec.Artifact.ExpectedSize); err == nil {
				cacheSpec.Size = size
			}
		}

		result, err := cacheClient.Ensure(transitionCtx, &cache.CacheRequest{
			Model:   model.Spec.Model.Name,
			Backend: string(model.Spec.Serving.Backend),
			Cache:   cacheSpec,
		})
		if err != nil {
			logger.Error(err, "cache ensure failed")
			activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
			setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "CacheFailed", err.Error())
			_ = r.Status().Update(ctx, activeModel)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Update cache state on model
		model.Status.CacheState = &cogitodevv1alpha1.CacheState{
			Location:     string(result),
			LastHydrated: &metav1.Time{Time: time.Now()},
		}
		setCondition(&model.Status, ArtifactCachedCondition, metav1.ConditionTrue, string(result), "Artifact cached")
		_ = r.Status().Update(ctx, model)
		logger.Info("cache ensure complete", "result", result)
	}

	// Step 3: Patch target backend deployment
	effectiveArgs := effectiveArgs(model)
	patchData := map[string]any{
		"spec": map[string]any{
			"replicas": int64(1),
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						activeModelAnno: model.Spec.Model.Name,
						switchedAtAnno:  time.Now().UTC().Format(time.RFC3339Nano),
					},
				},
				"spec": map[string]any{
					"containers": []map[string]any{
						{
							"name": backend.Spec.ContainerName,
							"args": effectiveArgs,
						},
					},
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal patch: %w", err)
	}

	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: backend.Spec.DeploymentRef.Name, Namespace: activeModel.Namespace}, &deployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("get deployment: %w", err)
	}

	if err := r.Patch(transitionCtx, &deployment, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		logger.Error(err, "failed to patch deployment")
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "PatchFailed", err.Error())
		_ = r.Status().Update(ctx, activeModel)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 4: Wait for rollout and health
	if err := r.waitForRollout(transitionCtx, backend.Spec.DeploymentRef.Name, activeModel.Namespace); err != nil {
		logger.Error(err, "timeout waiting for rollout")
		return ctrl.Result{RequeueAfter: backendProbeWait}, nil
	}

	// Step 5: Health check
	backendURL := fmt.Sprintf("http://%s:%d", backend.Spec.ServiceRef.Name, backend.Spec.Port)
	if !r.healthCheck(transitionCtx, backendURL) {
		logger.Info("backend not yet healthy, requeueing")
		return ctrl.Result{RequeueAfter: backendProbeWait}, nil
	}

	// Step 6: Collect runtime metadata
	runtimeMeta, err := r.collectRuntimeMetadata(transitionCtx, backendURL, model)
	if err != nil {
		logger.Error(err, "failed to collect runtime metadata")
		// Non-fatal, continue
	}

	// Step 7: Update model status to Active
	model.Status.Active = true
	model.Status.Phase = cogitodevv1alpha1.ModelPhaseActive
	if runtimeMeta != nil {
		model.Status.RuntimeMetadata = runtimeMeta
	}
	if err := r.Status().Update(ctx, model); err != nil {
		logger.Error(err, "failed to update model status")
	}

	// Step 8: Deactivate previous model
	if activeModel.Status.TransitionFrom != "" && activeModel.Status.TransitionFrom != model.Spec.Model.Name {
		prevModel, _ := r.findModel(ctx, activeModel.Namespace, activeModel.Status.TransitionFrom)
		if prevModel != nil {
			prevModel.Status.Active = false
			prevModel.Status.Phase = cogitodevv1alpha1.ModelPhaseReady
			_ = r.Status().Update(ctx, prevModel)
		}
	}

	// Step 9: Mark stable
	transitionDuration := time.Since(activeModel.Status.TransitionStarted.Time)
	activeModel.Status.ModelName = model.Spec.Model.Name
	activeModel.Status.BackendType = model.Spec.Serving.Backend
	activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseStable
	activeModel.Status.TransitionDuration = &metav1.Duration{Duration: transitionDuration}
	setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionTrue, "Active", "Model is active")
	setActiveCondition(&activeModel.Status, TransitionCompleteCond, metav1.ConditionTrue, "Complete", "Transition complete")

	if err := r.Status().Update(ctx, activeModel); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("transition complete", "model", model.Spec.Model.Name, "duration", transitionDuration)
	r.Recorder.Event(activeModel, corev1.EventTypeNormal, "TransitionComplete", fmt.Sprintf("Switched to model %s in %s", model.Spec.Model.Name, transitionDuration))

	return ctrl.Result{}, nil
}

func (r *LLMActiveModelReconciler) scaleDeployment(ctx context.Context, name, namespace string, replicas int32) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deployment); err != nil {
		return err
	}
	return r.Patch(ctx, &deployment, client.RawPatch(types.MergePatchType, patch))
}

func (r *LLMActiveModelReconciler) waitForScaleDown(ctx context.Context, name, namespace string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backendProbeWait):
		}

		var deployment appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deployment); err != nil {
			return err
		}
		if deployment.Status.Replicas == 0 && deployment.Status.AvailableReplicas == 0 {
			return nil
		}
	}
}

func (r *LLMActiveModelReconciler) waitForRollout(ctx context.Context, name, namespace string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backendProbeWait):
		}

		var deployment appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deployment); err != nil {
			return err
		}
		if deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.UpdatedReplicas == 1 &&
			deployment.Status.AvailableReplicas == 1 {
			return nil
		}
	}
}

func (r *LLMActiveModelReconciler) healthCheck(ctx context.Context, url string) bool {
	healthURL := url + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (r *LLMActiveModelReconciler) collectRuntimeMetadata(ctx context.Context, url string, model *cogitodevv1alpha1.LLMModel) (*cogitodevv1alpha1.RuntimeMetadata, error) {
	meta := &cogitodevv1alpha1.RuntimeMetadata{
		ObservedAt:      metav1.Now(),
		ContextLength:   model.Spec.Serving.MaxModelLen,
		LaunchArguments: launchArguments(effectiveArgs(model)),
	}

	// Get served models from /v1/models
	models, err := r.backendModels(ctx, url)
	if err == nil {
		meta.ServedModelIDs = models
	}

	// Get max-num-seqs from args
	if seqs, ok := meta.LaunchArguments["--max-num-seqs"]; ok {
		if n, err := strconv.Atoi(seqs); err == nil {
			meta.MaxConcurrentReqs = n
		}
	}

	// Get KV cache info from /metrics
	metrics, err := r.backendText(ctx, url+"/metrics")
	if err == nil {
		meta.KVCache = parseCacheConfig(metrics)
	}

	return meta, nil
}

func (r *LLMActiveModelReconciler) backendText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("backend returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (r *LLMActiveModelReconciler) backendModels(ctx context.Context, url string) ([]string, error) {
	body, err := r.backendText(ctx, url+"/v1/models")
	if err != nil {
		return nil, err
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range response.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func (r *LLMActiveModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.HTTPClient == nil {
		r.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if r.CacheManagerURL == "" {
		r.CacheManagerURL = os.Getenv("CACHE_MANAGER_URL")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&cogitodevv1alpha1.LLMActiveModel{}).
		Watches(&cogitodevv1alpha1.LLMModel{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(obj)}}
		})).
		Watches(&cogitodevv1alpha1.LLMBackend{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(obj)}}
		})).
		Complete(r)
}

func setActiveCondition(status *cogitodevv1alpha1.LLMActiveModelStatus, conditionType string, statusVal metav1.ConditionStatus, reason, message string) {
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

func launchArguments(args []string) map[string]string {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			values[args[i]] = args[i+1]
			i++
		} else {
			values[args[i]] = "true"
		}
	}
	return values
}

func parseCacheConfig(metrics string) map[string]string {
	scanner := bufio.NewScanner(strings.NewReader(metrics))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "vllm:cache_config_info{") {
			continue
		}
		start := strings.IndexByte(line, '{')
		end := strings.LastIndex(line, "}")
		if start < 0 || end <= start {
			return nil
		}
		return parsePrometheusLabels(line[start+1 : end])
	}
	return nil
}

func parsePrometheusLabels(encoded string) map[string]string {
	labels := map[string]string{}
	for len(encoded) > 0 {
		equals := strings.IndexByte(encoded, '=')
		if equals < 1 || equals+1 >= len(encoded) || encoded[equals+1] != '"' {
			return labels
		}
		key := encoded[:equals]
		encoded = encoded[equals+1:]
		end := 1
		for end < len(encoded) {
			if encoded[end] == '"' && encoded[end-1] != '\\' {
				break
			}
			end++
		}
		if end >= len(encoded) {
			return labels
		}
		value, err := strconv.Unquote(encoded[:end+1])
		if err != nil {
			return labels
		}
		labels[key] = value
		encoded = strings.TrimPrefix(encoded[end+1:], ",")
	}
	return labels
}

func parseSize(s string) (int64, error) {
	// Simple parser for sizes like "60Gi", "100Mi", "1G"
	var unitMultiplier int64
	switch {
	case strings.HasSuffix(s, "Gi"):
		unitMultiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "Gi")
	case strings.HasSuffix(s, "Mi"):
		unitMultiplier = 1024 * 1024
		s = strings.TrimSuffix(s, "Mi")
	case strings.HasSuffix(s, "Ki"):
		unitMultiplier = 1024
		s = strings.TrimSuffix(s, "Ki")
	case strings.HasSuffix(s, "G"):
		unitMultiplier = 1000 * 1000 * 1000
		s = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		unitMultiplier = 1000 * 1000
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		unitMultiplier = 1000
		s = strings.TrimSuffix(s, "K")
	default:
		unitMultiplier = 1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * unitMultiplier, nil
}

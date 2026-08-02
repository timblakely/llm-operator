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
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	logr "github.com/go-logr/logr"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
	runtimebackend "github.com/timblakely/llm-operator/internal/backend"
	"github.com/timblakely/llm-operator/internal/cache"
	"github.com/timblakely/llm-operator/internal/metrics"
)

const (
	activeModelAnno          = "llm.cogito.dev/active-model"
	switchedAtAnno           = "llm.cogito.dev/switched-at"
	chatTemplateAnno         = "llm.cogito.dev/chat-template-sha256"
	chatTemplateVolumeName   = "llm-chat-template"
	chatTemplateMountDir     = "/etc/llm-templates"
	chatTemplateMountFile    = "chat-template.jinja"
	backendProbeWait         = 2 * time.Second
	defaultTransitionTimeout = 30 * time.Minute
	errTransitionCancelled   = "transition cancelled"

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
	// ProbeInterval controls rollout and scale-down polling. The production
	// default is backendProbeWait; tests may use a shorter interval.
	ProbeInterval time.Duration
	// TransitionsEnabled prevents Deployment/cache mutation while the operator
	// is being evaluated beside the legacy proxy. It must be explicitly enabled.
	TransitionsEnabled bool
	// AllowedTransitionModels restricts enabled transitions to an explicit canary
	// set. An empty set permits all models for backwards compatibility.
	AllowedTransitionModels map[string]struct{}

	// transitionMu is a second line of defense around Deployment/cache mutation.
	// The controller is also configured with MaxConcurrentReconciles=1.
	transitionMu sync.Mutex
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

	owner, err := r.singletonOwner(ctx, activeModel.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owner != activeModel.Name {
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "DuplicateActiveModel", fmt.Sprintf("LLMActiveModel %q owns transitions in namespace %q", owner, activeModel.Namespace))
		if err := r.Status().Update(ctx, &activeModel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

	// A stable model can still need a rolling update when its desired runtime
	// contract changes (for example, a pinned chat template is added or changed).
	if activeModel.Status.ModelName == activeModel.Spec.ModelName &&
		activeModel.Status.BackendType == model.Spec.Serving.Backend {
		matches, err := r.deploymentMatchesModel(ctx, &activeModel, model, backend)
		if err != nil {
			return ctrl.Result{}, err
		}
		if matches {
			if activeModel.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseStable {
				activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseStable
				activeModel.Status.BackendType = model.Spec.Serving.Backend
				setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionTrue, "Active", "Model is active")
				setActiveCondition(&activeModel.Status, TransitionCompleteCond, metav1.ConditionTrue, "Complete", "Transition complete")
				if err := r.Status().Update(ctx, &activeModel); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
	}

	if !r.TransitionsEnabled {
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseTransitioning
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "TransitionsDisabled", "Model transitions are disabled on this controller manager")
		if err := r.Status().Update(ctx, &activeModel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if len(r.AllowedTransitionModels) > 0 {
		if _, allowed := r.AllowedTransitionModels[model.Spec.Model.Name]; !allowed {
			activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
			setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "CanaryDenied", fmt.Sprintf("model %q is not in the transition canary allowlist", model.Spec.Model.Name))
			if err := r.Status().Update(ctx, &activeModel); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// Execute transition
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	return r.executeTransition(ctx, &activeModel, model, backend, logger)
}

// singletonOwner returns the deterministic transition owner for a namespace.
// The oldest resource wins, with name as the tie-breaker. Non-owners receive a
// DuplicateActiveModel condition and periodically retry so deleting the owner
// allows a remaining resource to take over.
func (r *LLMActiveModelReconciler) singletonOwner(ctx context.Context, namespace string) (string, error) {
	var list cogitodevv1alpha1.LLMActiveModelList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "", nil
	}
	sort.Slice(list.Items, func(i, j int) bool {
		left := list.Items[i].CreationTimestamp.Time
		right := list.Items[j].CreationTimestamp.Time
		if left.Equal(right) {
			return list.Items[i].Name < list.Items[j].Name
		}
		return left.Before(right)
	})
	return list.Items[0].Name, nil
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

	startGen := activeModel.GetGeneration()

	// Persist a generation token before any external mutation. A Failed state,
	// a new generation, or an incomplete legacy status starts a fresh attempt.
	if activeModel.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseTransitioning ||
		activeModel.Status.TransitionGeneration != startGen ||
		activeModel.Status.TransitionStarted == nil {
		activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseTransitioning
		activeModel.Status.TransitionFrom = activeModel.Status.ModelName
		activeModel.Status.TransitionStarted = &metav1.Time{Time: time.Now()}
		activeModel.Status.TransitionGeneration = startGen
		setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, "Transitioning", "Transition in progress")
		if err := r.Status().Update(ctx, activeModel); err != nil {
			return ctrl.Result{}, err
		}
	}

	checkCurrent := func() error {
		return r.ensureTransitionCurrent(transitionCtx, activeModel, model.Spec.Model.Name, startGen)
	}
	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}
	driver, err := runtimebackend.DefaultRegistry().Driver(model.Spec.Serving.Backend)
	if err != nil {
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "UnsupportedBackend", "unsupported_backend", err)
	}
	if err := driver.Validate(model); err != nil {
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "InvalidServingConfiguration", "invalid_serving_configuration", err)
	}
	if err := validateChatTemplate(ctx, r.Client, model); err != nil {
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "TemplateInvalid", "template_invalid", err)
	}

	previousBackendType := activeModel.Status.BackendType
	if activeModel.Status.TransitionFrom != "" {
		if previousModel, err := r.findModel(ctx, activeModel.Namespace, activeModel.Status.TransitionFrom); err == nil {
			previousBackendType = previousModel.Spec.Serving.Backend
		}
	}

	// Step 1: Scale down current backend if different
	currentBackendType := previousBackendType
	if currentBackendType != "" && currentBackendType != model.Spec.Serving.Backend {
		currentBackend, err := r.findBackend(ctx, activeModel.Namespace, currentBackendType, nil)
		if err == nil && currentBackend != nil {
			if err := checkCurrent(); err != nil {
				return transitionCheckResult(err, logger)
			}
			if err := r.scaleDeployment(transitionCtx, currentBackend.Spec.DeploymentRef.Name, activeModel.Namespace, 0); err != nil {
				logger.Error(err, "failed to scale down current backend")
				return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "ScaleDownFailed", "scale_down_failed", err)
			}
			if err := r.waitForScaleDown(transitionCtx, currentBackend.Spec.DeploymentRef.Name, activeModel.Namespace, checkCurrent); err != nil {
				if errors.Is(err, errTransitionChanged) {
					return transitionCheckResult(err, logger)
				}
				logger.Error(err, "timeout waiting for current backend to scale down")
				return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "ScaleDownTimeout", "scale_down_timeout", err)
			}
			logger.Info("current backend scaled down", "backend", currentBackend.Name)
		}
	}

	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 2: Ensure cached using the target runtime's artifact format.
	cacheRequest, err := driver.CacheRequest(model)
	if err != nil {
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "InvalidCacheConfiguration", "invalid_cache_configuration", err)
	}
	if r.CacheManagerURL != "" && cacheRequest != nil {
		cacheClient := cache.NewWithHTTPClient(r.CacheManagerURL, r.httpClient())
		result, err := cacheClient.Ensure(transitionCtx, cacheRequest)
		if err != nil {
			logger.Error(err, "cache ensure failed")
			return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "CacheFailed", "cache_failed", err)
		}

		if err := checkCurrent(); err != nil {
			return transitionCheckResult(err, logger)
		}
		// Update cache state on model
		model.Status.CacheState = &cogitodevv1alpha1.CacheState{
			Location:     string(result),
			LastHydrated: &metav1.Time{Time: time.Now()},
		}
		setCondition(&model.Status, ArtifactCachedCondition, metav1.ConditionTrue, string(result), "Artifact cached")
		if err := r.Status().Update(ctx, model); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("cache ensure complete", "result", result)
	} else if r.CacheManagerURL == "" {
		logger.Info("cache-manager not configured, skipping cache step")
	}

	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 3: Patch only the selected container and preserve every sidecar.
	if err := r.activateDeployment(transitionCtx, activeModel, model, backend); err != nil {
		logger.Error(err, "failed to patch deployment")
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "PatchFailed", "patch_failed", err)
	}

	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 4: Wait for rollout and health
	if err := r.waitForRollout(transitionCtx, backend.Spec.DeploymentRef.Name, activeModel.Namespace, checkCurrent); err != nil {
		if errors.Is(err, errTransitionChanged) {
			return transitionCheckResult(err, logger)
		}
		logger.Error(err, "timeout waiting for rollout")
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "RolloutFailed", "rollout_failed", err)
	}

	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 5: Wait for runtime health. A Deployment can become available before
	// the runtime has accepted its first connection, so a single refused dial is
	// not a transition failure.
	backendURL := fmt.Sprintf("http://%s:%d", backend.Spec.ServiceRef.Name, backend.Spec.Port)
	if err := r.waitForBackendHealth(transitionCtx, driver, backendURL, checkCurrent); err != nil {
		logger.Error(err, "backend health check failed")
		return r.failTransition(ctx, activeModel, model.Spec.Model.Name, "HealthCheckFailed", "health_check_failed", err)
	}

	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 6: Collect runtime metadata
	runtimeMeta, err := driver.CollectRuntimeMetadata(transitionCtx, r.httpClient(), backendURL, model)
	if err != nil {
		logger.Error(err, "failed to collect runtime metadata")
		// Non-fatal, continue
	}
	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 7: Update model status to Active
	model.Status.Active = true
	model.Status.Phase = cogitodevv1alpha1.ModelPhaseActive
	if runtimeMeta != nil {
		model.Status.RuntimeMetadata = runtimeMeta
	}
	if err := r.Status().Update(ctx, model); err != nil {
		return ctrl.Result{}, fmt.Errorf("update target model status: %w", err)
	}
	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
	}

	// Step 8: Deactivate previous model
	if activeModel.Status.TransitionFrom != "" && activeModel.Status.TransitionFrom != model.Spec.Model.Name {
		prevModel, _ := r.findModel(ctx, activeModel.Namespace, activeModel.Status.TransitionFrom)
		if prevModel != nil {
			prevModel.Status.Active = false
			prevModel.Status.Phase = cogitodevv1alpha1.ModelPhaseReady
			if err := r.Status().Update(ctx, prevModel); err != nil {
				return ctrl.Result{}, fmt.Errorf("deactivate previous model: %w", err)
			}
		}
	}
	if err := checkCurrent(); err != nil {
		return transitionCheckResult(err, logger)
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

	// Record metrics
	metrics.RecordTransitionDuration(transitionDuration, model.Spec.Model.Name)
	metrics.RecordModelSwitch(activeModel.Status.TransitionFrom, model.Spec.Model.Name, string(model.Spec.Serving.Backend))

	logger.Info("transition complete", "model", model.Spec.Model.Name, "duration", transitionDuration)
	if r.Recorder != nil {
		r.Recorder.Event(activeModel, corev1.EventTypeNormal, "TransitionComplete", fmt.Sprintf("Switched to model %s in %s", model.Spec.Model.Name, transitionDuration))
	}

	return ctrl.Result{}, nil
}

func (r *LLMActiveModelReconciler) waitForBackendHealth(ctx context.Context, driver runtimebackend.Driver, backendURL string, checkCurrent func() error) error {
	var lastErr error
	for {
		if err := checkCurrent(); err != nil {
			return err
		}
		if err := driver.CheckHealth(ctx, r.httpClient(), backendURL); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("backend did not become healthy: %w", lastErr)
			}
			return ctx.Err()
		case <-time.After(r.probeInterval()):
		}
	}
}

var errTransitionChanged = errors.New(errTransitionCancelled)

func (r *LLMActiveModelReconciler) ensureTransitionCurrent(ctx context.Context, activeModel *cogitodevv1alpha1.LLMActiveModel, targetModel string, generation int64) error {
	var current cogitodevv1alpha1.LLMActiveModel
	key := types.NamespacedName{Name: activeModel.Name, Namespace: activeModel.Namespace}
	if err := r.Get(ctx, key, &current); err != nil {
		return err
	}
	if current.GetGeneration() != generation ||
		current.Spec.ModelName != targetModel ||
		current.Status.TransitionGeneration != generation {
		return fmt.Errorf("%w: requested model or generation changed", errTransitionChanged)
	}
	return nil
}

func transitionCheckResult(err error, logger logr.Logger) (ctrl.Result, error) {
	if errors.Is(err, errTransitionChanged) {
		logger.Info("transition cancelled because the requested generation changed")
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, err
}

func (r *LLMActiveModelReconciler) failTransition(ctx context.Context, activeModel *cogitodevv1alpha1.LLMActiveModel, modelName, reason, metricReason string, cause error) (ctrl.Result, error) {
	activeModel.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseFailed
	setActiveCondition(&activeModel.Status, ModelActiveCondition, metav1.ConditionFalse, reason, cause.Error())
	metrics.RecordTransitionFailure(modelName, metricReason)
	if err := r.Status().Update(ctx, activeModel); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *LLMActiveModelReconciler) activateDeployment(ctx context.Context, activeModel *cogitodevv1alpha1.LLMActiveModel, model *cogitodevv1alpha1.LLMModel, backend *cogitodevv1alpha1.LLMBackend) error {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: backend.Spec.DeploymentRef.Name, Namespace: activeModel.Namespace}, &deployment); err != nil {
		return fmt.Errorf("get deployment: %w", err)
	}
	base := deployment.DeepCopy()
	one := int32(1)
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != one {
		deployment.Spec.Replicas = &one
	}
	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].Name == backend.Spec.ContainerName {
			desiredArgs := effectiveArgs(model)
			argsChanged := !reflect.DeepEqual(deployment.Spec.Template.Spec.Containers[i].Args, desiredArgs)
			activeChanged := deployment.Spec.Template.Annotations == nil || deployment.Spec.Template.Annotations[activeModelAnno] != model.Spec.Model.Name
			templateChanged := applyChatTemplate(&deployment.Spec.Template.Spec, &deployment.Spec.Template.Spec.Containers[i], model.Spec.Serving.ChatTemplate)
			desiredTemplateDigest := ""
			if model.Spec.Serving.ChatTemplate != nil {
				desiredTemplateDigest = model.Spec.Serving.ChatTemplate.SHA256
			}
			currentTemplateDigest := ""
			if deployment.Spec.Template.Annotations != nil {
				currentTemplateDigest = deployment.Spec.Template.Annotations[chatTemplateAnno]
			}
			templateAnnotationChanged := currentTemplateDigest != desiredTemplateDigest
			if argsChanged || activeChanged || templateChanged || templateAnnotationChanged {
				if deployment.Spec.Template.Annotations == nil {
					deployment.Spec.Template.Annotations = make(map[string]string)
				}
				deployment.Spec.Template.Annotations[activeModelAnno] = model.Spec.Model.Name
				deployment.Spec.Template.Annotations[switchedAtAnno] = time.Now().UTC().Format(time.RFC3339Nano)
				if desiredTemplateDigest == "" {
					delete(deployment.Spec.Template.Annotations, chatTemplateAnno)
				} else {
					deployment.Spec.Template.Annotations[chatTemplateAnno] = desiredTemplateDigest
				}
				deployment.Spec.Template.Spec.Containers[i].Args = desiredArgs
			}
			if reflect.DeepEqual(base.Spec, deployment.Spec) {
				return nil
			}
			return r.Patch(ctx, &deployment, client.MergeFrom(base))
		}
	}
	return fmt.Errorf("container %q not found in deployment %q", backend.Spec.ContainerName, deployment.Name)
}

func (r *LLMActiveModelReconciler) deploymentMatchesModel(ctx context.Context, activeModel *cogitodevv1alpha1.LLMActiveModel, model *cogitodevv1alpha1.LLMModel, backend *cogitodevv1alpha1.LLMBackend) (bool, error) {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: backend.Spec.DeploymentRef.Name, Namespace: activeModel.Namespace}, &deployment); err != nil {
		return false, fmt.Errorf("get deployment: %w", err)
	}
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name != backend.Spec.ContainerName {
			continue
		}
		if !reflect.DeepEqual(container.Args, effectiveArgs(model)) || deployment.Spec.Template.Annotations[activeModelAnno] != model.Spec.Model.Name {
			return false, nil
		}
		if !chatTemplateMounted(&deployment.Spec.Template.Spec, container, model.Spec.Serving.ChatTemplate) {
			return false, nil
		}
		wantDigest := ""
		if model.Spec.Serving.ChatTemplate != nil {
			wantDigest = model.Spec.Serving.ChatTemplate.SHA256
		}
		return deployment.Spec.Template.Annotations[chatTemplateAnno] == wantDigest, nil
	}
	return false, fmt.Errorf("container %q not found in deployment %q", backend.Spec.ContainerName, deployment.Name)
}

// applyChatTemplate owns one reserved volume/mount pair on the runtime
// container. It returns whether it changed the Pod spec.
func applyChatTemplate(podSpec *corev1.PodSpec, container *corev1.Container, template *cogitodevv1alpha1.ChatTemplateSpec) bool {
	if template != nil && chatTemplateMounted(podSpec, container, template) {
		return false
	}
	beforeVolumes := append([]corev1.Volume(nil), podSpec.Volumes...)
	beforeMounts := append([]corev1.VolumeMount(nil), container.VolumeMounts...)

	volumes := podSpec.Volumes[:0]
	for _, volume := range podSpec.Volumes {
		if volume.Name != chatTemplateVolumeName {
			volumes = append(volumes, volume)
		}
	}
	podSpec.Volumes = volumes
	mounts := container.VolumeMounts[:0]
	for _, mount := range container.VolumeMounts {
		if mount.Name != chatTemplateVolumeName {
			mounts = append(mounts, mount)
		}
	}
	container.VolumeMounts = mounts

	if template != nil {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: chatTemplateVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: template.ConfigMapKeyRef.Name},
				Items:                []corev1.KeyToPath{{Key: template.ConfigMapKeyRef.Key, Path: chatTemplateMountFile}},
			}},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      chatTemplateVolumeName,
			MountPath: chatTemplateMountDir,
			ReadOnly:  true,
		})
	}
	return !reflect.DeepEqual(beforeVolumes, podSpec.Volumes) || !reflect.DeepEqual(beforeMounts, container.VolumeMounts)
}

// chatTemplateMounted compares only the controller-owned parts of the
// ConfigMap volume/mount. Kubernetes may default unrelated fields such as
// ConfigMapVolumeSource.DefaultMode after a Deployment is persisted.
func chatTemplateMounted(podSpec *corev1.PodSpec, container *corev1.Container, template *cogitodevv1alpha1.ChatTemplateSpec) bool {
	if template == nil {
		for _, volume := range podSpec.Volumes {
			if volume.Name == chatTemplateVolumeName {
				return false
			}
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == chatTemplateVolumeName {
				return false
			}
		}
		return true
	}

	var volume *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == chatTemplateVolumeName {
			if volume != nil {
				return false
			}
			volume = &podSpec.Volumes[i]
		}
	}
	if volume == nil || volume.ConfigMap == nil || volume.ConfigMap.Name != template.ConfigMapKeyRef.Name || len(volume.ConfigMap.Items) != 1 {
		return false
	}
	item := volume.ConfigMap.Items[0]
	if item.Key != template.ConfigMapKeyRef.Key || item.Path != chatTemplateMountFile {
		return false
	}

	for _, mount := range container.VolumeMounts {
		if mount.Name == chatTemplateVolumeName {
			return mount.MountPath == chatTemplateMountDir && mount.ReadOnly
		}
	}
	return false
}

func (r *LLMActiveModelReconciler) scaleDeployment(ctx context.Context, name, namespace string, replicas int32) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deployment); err != nil {
		return err
	}
	return r.Patch(ctx, &deployment, client.RawPatch(types.MergePatchType, patch))
}

func (r *LLMActiveModelReconciler) waitForScaleDown(ctx context.Context, name, namespace string, checkCurrent func() error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.probeInterval()):
		}
		if err := checkCurrent(); err != nil {
			return err
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

func (r *LLMActiveModelReconciler) waitForRollout(ctx context.Context, name, namespace string, checkCurrent func() error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.probeInterval()):
		}
		if err := checkCurrent(); err != nil {
			return err
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

func (r *LLMActiveModelReconciler) probeInterval() time.Duration {
	if r.ProbeInterval > 0 {
		return r.ProbeInterval
	}
	return backendProbeWait
}

func (r *LLMActiveModelReconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
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
		Watches(&cogitodevv1alpha1.LLMModel{}, handler.EnqueueRequestsFromMapFunc(
			r.enqueueActiveModelForModel,
		)).
		Watches(&cogitodevv1alpha1.LLMBackend{}, handler.EnqueueRequestsFromMapFunc(
			r.enqueueActiveModelForBackend,
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// enqueueActiveModelForModel finds the singleton LLMActiveModel in the same
// namespace as the changed LLMModel and enqueues it for reconciliation.
func (r *LLMActiveModelReconciler) enqueueActiveModelForModel(ctx context.Context, obj client.Object) []ctrl.Request {
	var list cogitodevv1alpha1.LLMActiveModelList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for _, am := range list.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(&am),
		})
	}
	return requests
}

// enqueueActiveModelForBackend finds the singleton LLMActiveModel in the same
// namespace as the changed LLMBackend and enqueues it for reconciliation.
func (r *LLMActiveModelReconciler) enqueueActiveModelForBackend(ctx context.Context, obj client.Object) []ctrl.Request {
	var list cogitodevv1alpha1.LLMActiveModelList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for _, am := range list.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(&am),
		})
	}
	return requests
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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestTransitionTargetContainerMissing(t *testing.T) {
	t.Parallel()

	model := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	backend := backendFor("vllm", "target-deployment", "missing", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, true)
	active := activeFor("active", model.Spec.Model.Name)
	reconciler, kubeClient := transitionReconciler(t, successHTTPClient(), deployment, model, backend, active)

	reconcileActive(t, reconciler, active)
	assertActiveFailure(t, kubeClient, active, "PatchFailed")
}

func TestTransitionCanaryAllowlistPreventsMutation(t *testing.T) {
	t.Parallel()

	model := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	backend := backendFor("vllm", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, true)
	active := activeFor("active", model.Spec.Model.Name)
	reconciler, kubeClient := transitionReconciler(t, successHTTPClient(), deployment, model, backend, active)
	reconciler.AllowedTransitionModels = map[string]struct{}{"acme/other": {}}

	reconcileActive(t, reconciler, active)
	assertActiveFailure(t, kubeClient, active, "CanaryDenied")

	var gotDeployment appsv1.Deployment
	getObject(t, kubeClient, deployment, &gotDeployment)
	if gotDeployment.Spec.Replicas == nil || *gotDeployment.Spec.Replicas != 0 {
		t.Fatalf("canary-denied transition changed replicas to %v", gotDeployment.Spec.Replicas)
	}
}

func TestTransitionCacheManagerUnavailable(t *testing.T) {
	t.Parallel()

	model := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	model.Spec.Artifact = &cogitodevv1alpha1.ArtifactSpec{ExpectedSize: "1Gi"}
	backend := backendFor("vllm", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, true)
	active := activeFor("active", model.Spec.Model.Name)
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return httpResponse(http.StatusServiceUnavailable, "cache offline"), nil
	})}
	reconciler, kubeClient := transitionReconciler(t, httpClient, deployment, model, backend, active)
	reconciler.CacheManagerURL = "http://cache-manager"

	reconcileActive(t, reconciler, active)
	assertActiveFailure(t, kubeClient, active, "CacheFailed")

	var got appsv1.Deployment
	getObject(t, kubeClient, deployment, &got)
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatalf("cache failure changed target replicas to %v", got.Spec.Replicas)
	}
}

func TestCacheHTTPClientUsesTransitionContextTimeout(t *testing.T) {
	t.Parallel()
	reconciler := &LLMActiveModelReconciler{HTTPClient: &http.Client{Timeout: 10 * time.Second}}
	if got := reconciler.cacheHTTPClient().Timeout; got != 0 {
		t.Fatalf("cache client timeout = %s, want 0", got)
	}
	if got := reconciler.httpClient().Timeout; got != 10*time.Second {
		t.Fatalf("runtime client timeout = %s, want unchanged", got)
	}
}

func TestSuccessfulTransitionCachesAndDeactivatesPreviousModel(t *testing.T) {
	t.Parallel()

	previous := modelFor("previous", "acme/previous", cogitodevv1alpha1.BackendVLLM)
	previous.Status.Active = true
	previous.Status.Phase = cogitodevv1alpha1.ModelPhaseActive
	target := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	target.Spec.Artifact = &cogitodevv1alpha1.ArtifactSpec{ExpectedSize: "1Gi"}
	backend := backendFor("vllm", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, true)
	active := activeFor("active", target.Spec.Model.Name)
	active.Status = cogitodevv1alpha1.LLMActiveModelStatus{
		ModelName:   previous.Spec.Model.Name,
		BackendType: cogitodevv1alpha1.BackendVLLM,
		Phase:       cogitodevv1alpha1.ActiveModelPhaseStable,
	}

	var cacheRequest struct {
		Model string `json:"model"`
	}
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/ensure" {
			if err := json.NewDecoder(req.Body).Decode(&cacheRequest); err != nil {
				return nil, err
			}
			response := httpResponse(http.StatusNoContent, "")
			response.Header.Set("X-LLM-Cache-Result", "hot")
			return response, nil
		}
		return successfulBackendResponse(req), nil
	})}
	reconciler, kubeClient := transitionReconciler(t, httpClient, deployment, previous, target, backend, active)
	reconciler.CacheManagerURL = "http://cache-manager"

	reconcileActive(t, reconciler, active)

	if cacheRequest.Model != target.Spec.Model.Name {
		t.Fatalf("cache request model = %q, want %q", cacheRequest.Model, target.Spec.Model.Name)
	}
	var gotTarget cogitodevv1alpha1.LLMModel
	getObject(t, kubeClient, target, &gotTarget)
	if !gotTarget.Status.Active || gotTarget.Status.Phase != cogitodevv1alpha1.ModelPhaseActive {
		t.Fatalf("target status = %#v, want active", gotTarget.Status)
	}
	if gotTarget.Status.CacheState == nil || gotTarget.Status.CacheState.Location != "hot" {
		t.Fatalf("target cache state = %#v, want hot", gotTarget.Status.CacheState)
	}
	var gotPrevious cogitodevv1alpha1.LLMModel
	getObject(t, kubeClient, previous, &gotPrevious)
	if gotPrevious.Status.Active || gotPrevious.Status.Phase != cogitodevv1alpha1.ModelPhaseReady {
		t.Fatalf("previous status = %#v, want inactive/Ready", gotPrevious.Status)
	}
	var gotActive cogitodevv1alpha1.LLMActiveModel
	getObject(t, kubeClient, active, &gotActive)
	if gotActive.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseStable || gotActive.Status.ModelName != target.Spec.Model.Name {
		t.Fatalf("active status = %#v, want stable target", gotActive.Status)
	}
}

func TestTransitionRolloutTimeout(t *testing.T) {
	t.Parallel()

	model := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	backend := backendFor("vllm", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, false)
	active := activeFor("active", model.Spec.Model.Name)
	reconciler, kubeClient := transitionReconciler(t, successHTTPClient(), deployment, model, backend, active)
	reconciler.TransitionTimeout = 15 * time.Millisecond

	reconcileActive(t, reconciler, active)
	assertActiveFailure(t, kubeClient, active, "RolloutFailed")
}

func TestTransitionHealthCheckFailure(t *testing.T) {
	t.Parallel()

	model := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	backend := backendFor("vllm", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, true)
	active := activeFor("active", model.Spec.Model.Name)
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return httpResponse(http.StatusServiceUnavailable, "not ready"), nil
	})}
	reconciler, kubeClient := transitionReconciler(t, httpClient, deployment, model, backend, active)

	reconcileActive(t, reconciler, active)
	assertActiveFailure(t, kubeClient, active, "HealthCheckFailed")
}

func TestTransitionScaleDownFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		previousDeployment *appsv1.Deployment
		reason             string
	}{
		{name: "deployment missing", reason: "ScaleDownFailed"},
		{name: "timeout", previousDeployment: deploymentFor("previous-deployment", "llama", 1, true), reason: "ScaleDownTimeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := modelFor("previous", "acme/previous", cogitodevv1alpha1.BackendLlamaCpp)
			target := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
			previousBackend := backendFor("previous-backend", "previous-deployment", "llama", cogitodevv1alpha1.BackendLlamaCpp)
			targetBackend := backendFor("target-backend", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
			targetDeployment := deploymentFor("target-deployment", "vllm", 0, true)
			active := activeFor("active", target.Spec.Model.Name)
			active.Status = cogitodevv1alpha1.LLMActiveModelStatus{
				ModelName:   previous.Spec.Model.Name,
				BackendType: cogitodevv1alpha1.BackendLlamaCpp,
				Phase:       cogitodevv1alpha1.ActiveModelPhaseStable,
			}
			objects := []client.Object{previous, target, previousBackend, targetBackend, targetDeployment, active}
			if tt.previousDeployment != nil {
				objects = append(objects, tt.previousDeployment)
			}
			reconciler, kubeClient := transitionReconciler(t, successHTTPClient(), objects...)
			reconciler.TransitionTimeout = 15 * time.Millisecond

			reconcileActive(t, reconciler, active)
			assertActiveFailure(t, kubeClient, active, tt.reason)
		})
	}
}

func TestCrossBackendScaleDownOrdering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		from    cogitodevv1alpha1.BackendType
		to      cogitodevv1alpha1.BackendType
		fromCtr string
		toCtr   string
	}{
		{name: "vllm to llama.cpp", from: cogitodevv1alpha1.BackendVLLM, to: cogitodevv1alpha1.BackendLlamaCpp, fromCtr: "vllm", toCtr: "llama"},
		{name: "llama.cpp to vllm", from: cogitodevv1alpha1.BackendLlamaCpp, to: cogitodevv1alpha1.BackendVLLM, fromCtr: "llama", toCtr: "vllm"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := modelFor("previous", "acme/previous", tt.from)
			target := modelFor("target", "acme/target", tt.to)
			fromBackend := backendFor("from-backend", "from-deployment", tt.fromCtr, tt.from)
			toBackend := backendFor("to-backend", "to-deployment", tt.toCtr, tt.to)
			fromDeployment := deploymentFor("from-deployment", tt.fromCtr, 1, false)
			toDeployment := deploymentFor("to-deployment", tt.toCtr, 0, true)
			active := activeFor("active", target.Spec.Model.Name)
			active.Status = cogitodevv1alpha1.LLMActiveModelStatus{
				ModelName:   previous.Spec.Model.Name,
				BackendType: tt.from,
				Phase:       cogitodevv1alpha1.ActiveModelPhaseStable,
			}

			reconciler, baseClient := transitionReconciler(t, successHTTPClient(), fromDeployment, toDeployment, previous, target, fromBackend, toBackend, active)
			recorder := &deploymentPatchRecorder{Client: baseClient}
			reconciler.Client = recorder
			reconcileActive(t, reconciler, active)

			patches := recorder.snapshot()
			if len(patches) < 2 {
				t.Fatalf("deployment patches = %#v, want scale-down then activation", patches)
			}
			if patches[0] != (deploymentPatch{name: fromDeployment.Name, replicas: 0}) {
				t.Fatalf("first patch = %#v, want scale-down of %s", patches[0], fromDeployment.Name)
			}
			if patches[1] != (deploymentPatch{name: toDeployment.Name, replicas: 1}) {
				t.Fatalf("second patch = %#v, want activation of %s", patches[1], toDeployment.Name)
			}
		})
	}
}

func TestSameRuntimeBackendsScaleDownOrdering(t *testing.T) {
	t.Parallel()
	previous := modelFor("laguna", "acme/laguna", cogitodevv1alpha1.BackendLlamaCpp)
	target := modelFor("deepseek", "acme/deepseek", cogitodevv1alpha1.BackendLlamaCpp)
	previous.Spec.BackendRef = &corev1.LocalObjectReference{Name: "laguna"}
	target.Spec.BackendRef = &corev1.LocalObjectReference{Name: "deepseek"}
	fromBackend := backendFor("laguna", "laguna-deployment", "laguna", cogitodevv1alpha1.BackendLlamaCpp)
	toBackend := backendFor("deepseek", "deepseek-deployment", "deepseek", cogitodevv1alpha1.BackendLlamaCpp)
	fromDeployment := deploymentFor("laguna-deployment", "laguna", 1, false)
	toDeployment := deploymentFor("deepseek-deployment", "deepseek", 0, true)
	active := activeFor("active", target.Spec.Model.Name)
	active.Status = cogitodevv1alpha1.LLMActiveModelStatus{ModelName: previous.Spec.Model.Name, BackendType: cogitodevv1alpha1.BackendLlamaCpp, Phase: cogitodevv1alpha1.ActiveModelPhaseStable}

	reconciler, baseClient := transitionReconciler(t, successHTTPClient(), fromDeployment, toDeployment, previous, target, fromBackend, toBackend, active)
	recorder := &deploymentPatchRecorder{Client: baseClient}
	reconciler.Client = recorder
	reconcileActive(t, reconciler, active)
	patches := recorder.snapshot()
	if len(patches) < 2 || patches[0] != (deploymentPatch{name: fromDeployment.Name, replicas: 0}) || patches[1] != (deploymentPatch{name: toDeployment.Name, replicas: 1}) {
		t.Fatalf("deployment patches = %#v, want Laguna scale-down then DeepSeek activation", patches)
	}
}

func TestChangedModelCancelsTransitionBeforeActivation(t *testing.T) {
	t.Parallel()

	target := modelFor("target", "acme/target", cogitodevv1alpha1.BackendVLLM)
	target.Spec.Artifact = &cogitodevv1alpha1.ArtifactSpec{ExpectedSize: "1Gi"}
	replacement := modelFor("replacement", "acme/replacement", cogitodevv1alpha1.BackendVLLM)
	backend := backendFor("vllm", "target-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
	deployment := deploymentFor("target-deployment", "vllm", 0, true)
	active := activeFor("active", target.Spec.Model.Name)

	var kubeClient client.Client
	var once sync.Once
	httpClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/ensure" {
			once.Do(func() {
				var current cogitodevv1alpha1.LLMActiveModel
				getObject(t, kubeClient, active, &current)
				current.Spec.ModelName = replacement.Spec.Model.Name
				current.Generation++
				if err := kubeClient.Update(context.Background(), &current); err != nil {
					t.Errorf("change active model during cache request: %v", err)
				}
			})
			response := httpResponse(http.StatusNoContent, "")
			response.Header.Set("X-LLM-Cache-Result", "hot")
			return response, nil
		}
		return successfulBackendResponse(req), nil
	})}
	reconciler, kubeClientFromBuilder := transitionReconciler(t, httpClient, deployment, target, replacement, backend, active)
	kubeClient = kubeClientFromBuilder
	reconciler.CacheManagerURL = "http://cache-manager"

	result := reconcileActive(t, reconciler, active)
	if !result.Requeue {
		t.Fatalf("cancelled transition result = %#v, want immediate requeue", result)
	}
	var gotDeployment appsv1.Deployment
	getObject(t, kubeClient, deployment, &gotDeployment)
	if gotDeployment.Spec.Replicas == nil || *gotDeployment.Spec.Replicas != 0 {
		t.Fatalf("cancelled transition activated deployment: replicas=%v", gotDeployment.Spec.Replicas)
	}
	if gotDeployment.Spec.Template.Annotations[activeModelAnno] != "" {
		t.Fatalf("cancelled transition wrote active-model annotation %q", gotDeployment.Spec.Template.Annotations[activeModelAnno])
	}
}

func TestConcurrentReconcilesSerializeDeploymentMutation(t *testing.T) {
	t.Parallel()

	objects := make([]client.Object, 0, 6)
	actives := make([]*cogitodevv1alpha1.LLMActiveModel, 0, 2)
	for _, namespace := range []string{"llm-a", "llm-b"} {
		model := modelFor(namespace+"-model", "acme/"+namespace, cogitodevv1alpha1.BackendVLLM)
		model.Namespace = namespace
		backend := backendFor(namespace+"-backend", namespace+"-deployment", "vllm", cogitodevv1alpha1.BackendVLLM)
		backend.Namespace = namespace
		deployment := deploymentFor(namespace+"-deployment", "vllm", 0, true)
		deployment.Namespace = namespace
		active := activeFor("active", model.Spec.Model.Name)
		active.Namespace = namespace
		objects = append(objects, model, backend, deployment, active)
		actives = append(actives, active)
	}

	reconciler, baseClient := transitionReconciler(t, successHTTPClient(), objects...)
	blockingClient := newBlockingPatchClient(baseClient)
	reconciler.Client = blockingClient

	errorsCh := make(chan error, 2)
	go func() {
		_, err := reconciler.Reconcile(context.Background(), requestFor(actives[0]))
		errorsCh <- err
	}()
	select {
	case <-blockingClient.entered:
	case <-time.After(time.Second):
		t.Fatal("first reconcile did not reach deployment mutation")
	}
	go func() {
		_, err := reconciler.Reconcile(context.Background(), requestFor(actives[1]))
		errorsCh <- err
	}()
	select {
	case <-blockingClient.entered:
		t.Fatal("second reconcile mutated a deployment while the first mutation was blocked")
	case <-time.After(25 * time.Millisecond):
	}
	close(blockingClient.release)
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	if got := blockingClient.maxConcurrent.Load(); got != 1 {
		t.Fatalf("maximum concurrent deployment patches = %d, want 1", got)
	}
}

func transitionReconciler(t *testing.T, httpClient *http.Client, objects ...client.Object) (*LLMActiveModelReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
	for _, object := range objects {
		switch object.(type) {
		case *cogitodevv1alpha1.LLMActiveModel, *cogitodevv1alpha1.LLMModel, *cogitodevv1alpha1.LLMBackend, *appsv1.Deployment:
			builder = builder.WithStatusSubresource(object)
		}
	}
	kubeClient := builder.Build()
	return &LLMActiveModelReconciler{
		Client:             kubeClient,
		Scheme:             scheme,
		Recorder:           record.NewFakeRecorder(10),
		HTTPClient:         httpClient,
		ProbeInterval:      time.Millisecond,
		TransitionTimeout:  100 * time.Millisecond,
		TransitionsEnabled: true,
	}, kubeClient
}

func reconcileActive(t *testing.T, reconciler *LLMActiveModelReconciler, active *cogitodevv1alpha1.LLMActiveModel) ctrl.Result {
	t.Helper()
	result, err := reconciler.Reconcile(context.Background(), requestFor(active))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func requestFor(active *cogitodevv1alpha1.LLMActiveModel) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: active.Name, Namespace: active.Namespace}}
}

func assertActiveFailure(t *testing.T, kubeClient client.Client, active *cogitodevv1alpha1.LLMActiveModel, reason string) {
	t.Helper()
	var got cogitodevv1alpha1.LLMActiveModel
	getObject(t, kubeClient, active, &got)
	if got.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseFailed {
		t.Fatalf("phase = %q, want Failed; status=%#v", got.Status.Phase, got.Status)
	}
	if !hasActiveCondition(got.Status.Conditions, reason) {
		t.Fatalf("missing %s condition: %#v", reason, got.Status.Conditions)
	}
}

func getObject(t *testing.T, kubeClient client.Client, reference client.Object, target client.Object) {
	t.Helper()
	if err := kubeClient.Get(context.Background(), client.ObjectKeyFromObject(reference), target); err != nil {
		t.Fatal(err)
	}
}

func modelFor(objectName, canonicalName string, backendType cogitodevv1alpha1.BackendType) *cogitodevv1alpha1.LLMModel {
	return &cogitodevv1alpha1.LLMModel{
		ObjectMeta: metav1.ObjectMeta{Name: objectName, Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMModelSpec{
			Model: cogitodevv1alpha1.LLMModelRef{Name: canonicalName, Source: canonicalName},
			Serving: cogitodevv1alpha1.ServingSpec{
				Backend:     backendType,
				DisplayName: canonicalName,
				MaxModelLen: 8192,
				Args:        []string{},
			},
		},
	}
}

func backendFor(name, deploymentName, containerName string, backendType cogitodevv1alpha1.BackendType) *cogitodevv1alpha1.LLMBackend {
	return &cogitodevv1alpha1.LLMBackend{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm"},
		Spec: cogitodevv1alpha1.LLMBackendSpec{
			Type:          backendType,
			DeploymentRef: corev1.LocalObjectReference{Name: deploymentName},
			ContainerName: containerName,
			ServiceRef:    corev1.LocalObjectReference{Name: deploymentName},
			Port:          8000,
		},
	}
}

func deploymentFor(name, containerName string, replicas int32, rolloutReady bool) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Args: []string{"--old-model"}}}}},
		},
	}
	if rolloutReady {
		deployment.Status.ObservedGeneration = 1
		deployment.Status.Replicas = 1
		deployment.Status.UpdatedReplicas = 1
		deployment.Status.AvailableReplicas = 1
	}
	return deployment
}

func activeFor(name, modelName string) *cogitodevv1alpha1.LLMActiveModel {
	return &cogitodevv1alpha1.LLMActiveModel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "llm", Generation: 1},
		Spec:       cogitodevv1alpha1.LLMActiveModelSpec{ModelName: modelName},
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func successHTTPClient() *http.Client {
	return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return successfulBackendResponse(req), nil
	})}
}

func successfulBackendResponse(req *http.Request) *http.Response {
	switch req.URL.Path {
	case "/health", "/healthz", "/health_generate":
		return httpResponse(http.StatusOK, "ok")
	case "/v1/models":
		return httpResponse(http.StatusOK, `{"data":[{"id":"acme/model"}]}`)
	case "/metrics":
		return httpResponse(http.StatusOK, "")
	default:
		return httpResponse(http.StatusNotFound, "not found")
	}
}

func httpResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type deploymentPatch struct {
	name     string
	replicas int32
}

type deploymentPatchRecorder struct {
	client.Client
	mu      sync.Mutex
	patches []deploymentPatch
}

func (c *deploymentPatchRecorder) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if err := c.Client.Patch(ctx, object, patch, options...); err != nil {
		return err
	}
	if deployment, ok := object.(*appsv1.Deployment); ok {
		var current appsv1.Deployment
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(deployment), &current); err != nil {
			return err
		}
		replicas := int32(0)
		if current.Spec.Replicas != nil {
			replicas = *current.Spec.Replicas
		}
		c.mu.Lock()
		c.patches = append(c.patches, deploymentPatch{name: current.Name, replicas: replicas})
		c.mu.Unlock()
	}
	return nil
}

func (c *deploymentPatchRecorder) snapshot() []deploymentPatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]deploymentPatch(nil), c.patches...)
}

type blockingPatchClient struct {
	client.Client
	entered       chan struct{}
	release       chan struct{}
	concurrent    atomic.Int32
	maxConcurrent atomic.Int32
}

func newBlockingPatchClient(kubeClient client.Client) *blockingPatchClient {
	return &blockingPatchClient{
		Client:  kubeClient,
		entered: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
}

func (c *blockingPatchClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if _, ok := object.(*appsv1.Deployment); !ok {
		return c.Client.Patch(ctx, object, patch, options...)
	}
	current := c.concurrent.Add(1)
	defer c.concurrent.Add(-1)
	for {
		maximum := c.maxConcurrent.Load()
		if current <= maximum || c.maxConcurrent.CompareAndSwap(maximum, current) {
			break
		}
	}
	c.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

var _ client.Client = (*deploymentPatchRecorder)(nil)
var _ client.Client = (*blockingPatchClient)(nil)

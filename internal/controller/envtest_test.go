//go:build envtest

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

var (
	envConfig *rest.Config
	envClient client.Client
	envScheme *runtime.Scheme
	testEnv   *envtest.Environment
)

func TestMain(m *testing.M) {
	envScheme = runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(envScheme); err != nil {
		panic(err)
	}
	if err := cogitodevv1alpha1.AddToScheme(envScheme); err != nil {
		panic(err)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd"},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	envConfig, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		os.Exit(1)
	}
	envClient, err = client.New(envConfig, client.Options{Scheme: envScheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create envtest client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestEnvtestStatusSubresourceIsolation(t *testing.T) {
	namespace := createEnvNamespace(t)
	active := activeFor("active", "acme/model")
	active.Namespace = namespace
	if err := envClient.Create(context.Background(), active); err != nil {
		t.Fatal(err)
	}

	normalUpdate := active.DeepCopy()
	normalUpdate.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseStable
	if err := envClient.Update(context.Background(), normalUpdate); err != nil {
		t.Fatal(err)
	}
	var got cogitodevv1alpha1.LLMActiveModel
	getEnvObject(t, active, &got)
	if got.Status.Phase != "" {
		t.Fatalf("normal update changed status subresource to %q", got.Status.Phase)
	}

	got.Status.Phase = cogitodevv1alpha1.ActiveModelPhaseTransitioning
	got.Status.TransitionGeneration = got.Generation
	if err := envClient.Status().Update(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	getEnvObject(t, active, &got)
	if got.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseTransitioning || got.Status.TransitionGeneration != got.Generation {
		t.Fatalf("status update was not persisted: %#v", got.Status)
	}
}

func TestEnvtestModelFinalizerProtectsActiveModel(t *testing.T) {
	namespace := createEnvNamespace(t)
	backend := backendFor("vllm", "backend", "vllm", cogitodevv1alpha1.BackendVLLM)
	backend.Namespace = namespace
	if err := envClient.Create(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	model := modelFor("model", "acme/model", cogitodevv1alpha1.BackendVLLM)
	model.Namespace = namespace
	if err := envClient.Create(context.Background(), model); err != nil {
		t.Fatal(err)
	}

	reconciler := &LLMModelReconciler{Client: envClient, Scheme: envScheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(model)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var current cogitodevv1alpha1.LLMModel
	getEnvObject(t, model, &current)
	if !containsString(current.Finalizers, cogitodevv1alpha1.ModelProtectionFinalizer) {
		t.Fatalf("model finalizers = %v, missing protection finalizer", current.Finalizers)
	}

	current.Status.Active = true
	if err := envClient.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if err := envClient.Delete(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("deleting an active model unexpectedly removed its finalizer")
	}
	getEnvObject(t, model, &current)
	if current.DeletionTimestamp.IsZero() {
		t.Fatal("active model was not retained in terminating state")
	}

	current.Status.Active = false
	if err := envClient.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		err := envClient.Get(context.Background(), client.ObjectKeyFromObject(model), &cogitodevv1alpha1.LLMModel{})
		return apierrors.IsNotFound(err)
	}, "inactive model deletion")
}

func TestEnvtestActiveModelWatchesModelAndBackend(t *testing.T) {
	namespace := createEnvNamespace(t)
	mgr, err := ctrl.NewManager(envConfig, ctrl.Options{
		Scheme:                 envScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &LLMActiveModelReconciler{
		Client:             mgr.GetClient(),
		Scheme:             envScheme,
		TransitionsEnabled: false,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	managerCtx, cancelManager := context.WithCancel(context.Background())
	managerDone := make(chan error, 1)
	go func() { managerDone <- mgr.Start(managerCtx) }()
	if !mgr.GetCache().WaitForCacheSync(managerCtx) {
		cancelManager()
		t.Fatal("manager cache did not sync")
	}
	t.Cleanup(func() {
		cancelManager()
		select {
		case err := <-managerDone:
			if err != nil {
				t.Errorf("manager stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	})

	active := activeFor("active", "acme/model")
	active.Namespace = namespace
	if err := envClient.Create(context.Background(), active); err != nil {
		t.Fatal(err)
	}
	waitForActiveReason(t, active, "ModelNotFound")

	model := modelFor("model", active.Spec.ModelName, cogitodevv1alpha1.BackendVLLM)
	model.Namespace = namespace
	if err := envClient.Create(context.Background(), model); err != nil {
		t.Fatal(err)
	}
	waitForActiveReason(t, active, "BackendNotFound")

	backend := backendFor("vllm", "backend", "vllm", cogitodevv1alpha1.BackendVLLM)
	backend.Namespace = namespace
	if err := envClient.Create(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	waitForActiveReason(t, active, "TransitionsDisabled")
}

func TestEnvtestSingletonOwnerAndTakeover(t *testing.T) {
	namespace := createEnvNamespace(t)
	model := modelFor("model", "acme/model", cogitodevv1alpha1.BackendVLLM)
	model.Namespace = namespace
	backend := backendFor("vllm", "backend", "vllm", cogitodevv1alpha1.BackendVLLM)
	backend.Namespace = namespace
	for _, object := range []client.Object{model, backend} {
		if err := envClient.Create(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	owner := activeFor("a-owner", model.Spec.Model.Name)
	owner.Namespace = namespace
	duplicate := activeFor("b-duplicate", model.Spec.Model.Name)
	duplicate.Namespace = namespace
	if err := envClient.Create(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	// Ensure creation timestamps cannot tie on servers with coarse clocks.
	time.Sleep(5 * time.Millisecond)
	if err := envClient.Create(context.Background(), duplicate); err != nil {
		t.Fatal(err)
	}

	reconciler := &LLMActiveModelReconciler{Client: envClient, Scheme: envScheme, TransitionsEnabled: false}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(duplicate)}); err != nil {
		t.Fatal(err)
	}
	var gotDuplicate cogitodevv1alpha1.LLMActiveModel
	getEnvObject(t, duplicate, &gotDuplicate)
	if gotDuplicate.Status.Phase != cogitodevv1alpha1.ActiveModelPhaseFailed || !hasActiveCondition(gotDuplicate.Status.Conditions, "DuplicateActiveModel") {
		t.Fatalf("duplicate status = %#v, want DuplicateActiveModel failure", gotDuplicate.Status)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)}); err != nil {
		t.Fatal(err)
	}
	var gotOwner cogitodevv1alpha1.LLMActiveModel
	getEnvObject(t, owner, &gotOwner)
	if !hasActiveCondition(gotOwner.Status.Conditions, "TransitionsDisabled") {
		t.Fatalf("owner did not reconcile: %#v", gotOwner.Status)
	}

	if err := envClient.Delete(context.Background(), &gotOwner); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		err := envClient.Get(context.Background(), client.ObjectKeyFromObject(owner), &cogitodevv1alpha1.LLMActiveModel{})
		return apierrors.IsNotFound(err)
	}, "singleton owner deletion")
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(duplicate)}); err != nil {
		t.Fatal(err)
	}
	getEnvObject(t, duplicate, &gotDuplicate)
	if !hasActiveCondition(gotDuplicate.Status.Conditions, "TransitionsDisabled") {
		t.Fatalf("remaining singleton did not take over: %#v", gotDuplicate.Status)
	}
}

func createEnvNamespace(t *testing.T) string {
	t.Helper()
	name := "envtest-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := envClient.Create(context.Background(), namespace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := envClient.Delete(context.Background(), namespace); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete namespace: %v", err)
		}
	})
	return name
}

func getEnvObject(t *testing.T, reference client.Object, target client.Object) {
	t.Helper()
	if err := envClient.Get(context.Background(), client.ObjectKeyFromObject(reference), target); err != nil {
		t.Fatal(err)
	}
}

func waitForActiveReason(t *testing.T, active *cogitodevv1alpha1.LLMActiveModel, reason string) {
	t.Helper()
	eventually(t, func() bool {
		var current cogitodevv1alpha1.LLMActiveModel
		if err := envClient.Get(context.Background(), client.ObjectKeyFromObject(active), &current); err != nil {
			return false
		}
		return hasActiveCondition(current.Status.Conditions, reason)
	}, reason+" condition")
}

func eventually(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// Package admission contains Kubernetes admission validation for LLM resources.
package admission

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	admission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
	"github.com/timblakely/llm-operator/internal/backend"
)

// LLMModelValidator rejects duplicate canonical models, unavailable backends,
// and serving arguments that the selected runtime cannot safely accept.
type LLMModelValidator struct {
	Reader client.Reader
}

func (v *LLMModelValidator) ValidateCreate(ctx context.Context, model *cogitodevv1alpha1.LLMModel) (admission.Warnings, error) {
	return nil, v.validate(ctx, model)
}

func (v *LLMModelValidator) ValidateUpdate(ctx context.Context, _ *cogitodevv1alpha1.LLMModel, model *cogitodevv1alpha1.LLMModel) (admission.Warnings, error) {
	return nil, v.validate(ctx, model)
}

func (v *LLMModelValidator) ValidateDelete(context.Context, *cogitodevv1alpha1.LLMModel) (admission.Warnings, error) {
	return nil, nil
}

func (v *LLMModelValidator) validate(ctx context.Context, model *cogitodevv1alpha1.LLMModel) error {
	driver, err := backend.DefaultRegistry().Driver(model.Spec.Serving.Backend)
	if err != nil {
		return err
	}
	if err := driver.Validate(model); err != nil {
		return err
	}
	if v.Reader == nil {
		return nil
	}

	var models cogitodevv1alpha1.LLMModelList
	if err := v.Reader.List(ctx, &models, client.InNamespace(model.Namespace)); err != nil {
		return fmt.Errorf("list models for canonical-name validation: %w", err)
	}
	for _, existing := range models.Items {
		if existing.Name != model.Name && existing.Spec.Model.Name == model.Spec.Model.Name {
			return fmt.Errorf("canonical model name %q is already owned by LLMModel %q", model.Spec.Model.Name, existing.Name)
		}
	}

	if model.Spec.BackendRef != nil {
		var target cogitodevv1alpha1.LLMBackend
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: model.Namespace, Name: model.Spec.BackendRef.Name}, &target); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced backend %q does not exist", model.Spec.BackendRef.Name)
			}
			return fmt.Errorf("get referenced backend: %w", err)
		}
		if target.Spec.Type != model.Spec.Serving.Backend {
			return fmt.Errorf("referenced backend %q has type %q, want %q", target.Name, target.Spec.Type, model.Spec.Serving.Backend)
		}
	}
	return nil
}

package migration

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

func TestConvertConfigMaps(t *testing.T) {
	configMaps := []corev1.ConfigMap{
		{
			ObjectMeta: objectMeta("llm-overlay-agentic", ModelOverlayLabel),
			Data: map[string]string{
				"model_name": "agentic", "display_name": "Agentic", "base_model": "acme/model",
				"request_defaults.json": `{"thinking":true}`,
			},
		},
		{
			ObjectMeta: objectMeta("llm-model-acme", ModelConfigLabel),
			Data: map[string]string{"model.yaml": `
model:
  name: acme/model
  source: acme/model
  revision: abc123
serving:
  backend: vllm
  displayName: Acme
  maxModelLen: 8192
  args: [--dtype, float16]
`},
		},
	}

	objects, err := ConvertConfigMaps(configMaps)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("converted %d objects, want 2", len(objects))
	}
	model, ok := objects[0].(*cogitodevv1alpha1.LLMModel)
	if !ok {
		t.Fatalf("first object = %T, want LLMModel", objects[0])
	}
	if model.Name != "llm-model-acme" || model.Spec.Model.Name != "acme/model" || model.Spec.Serving.Backend != cogitodevv1alpha1.BackendVLLM {
		t.Fatalf("model = %#v", model)
	}
	if model.Annotations[MigratedFromAnnotation] != "llm-model-acme" {
		t.Fatalf("annotations = %#v", model.Annotations)
	}
	overlay, ok := objects[1].(*cogitodevv1alpha1.LLMModelOverlay)
	if !ok {
		t.Fatalf("second object = %T, want LLMModelOverlay", objects[1])
	}
	if overlay.Name != "agentic" || overlay.Spec.BaseModel != "acme/model" {
		t.Fatalf("overlay = %#v", overlay)
	}
	var defaults map[string]bool
	if err := json.Unmarshal(overlay.Spec.RequestDefaults.Raw, &defaults); err != nil {
		t.Fatalf("request defaults are not JSON: %v", err)
	}
	if !defaults["thinking"] {
		t.Fatalf("request defaults = %#v", defaults)
	}
}

func TestConvertConfigMapsRejectsInvalidLabeledInput(t *testing.T) {
	_, err := ConvertConfigMaps([]corev1.ConfigMap{{ObjectMeta: objectMeta("bad", ModelConfigLabel)}})
	if err == nil || !strings.Contains(err.Error(), "missing data.model.yaml") {
		t.Fatalf("error = %v, want missing model.yaml", err)
	}
}

func TestDecodeConfigMapsRejectsNonConfigMap(t *testing.T) {
	_, err := DecodeConfigMaps([]byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: no\n"))
	if err == nil || !strings.Contains(err.Error(), "want v1 ConfigMap") {
		t.Fatalf("error = %v, want non-ConfigMap error", err)
	}
}

func objectMeta(name, label string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: "llm",
		Labels: map[string]string{label: "true"},
	}
}

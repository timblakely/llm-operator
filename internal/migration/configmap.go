// Package migration converts the legacy proxy ConfigMap catalog into the LLM
// custom resources used by the operator.
package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	cogitodevv1alpha1 "github.com/timblakely/llm-operator/api/cogito.dev/v1alpha1"
)

const (
	ModelConfigLabel       = "llm.cogito.dev/model-config"
	ModelOverlayLabel      = "llm.cogito.dev/model-overlay"
	MigratedFromAnnotation = "llm.cogito.dev/migrated-from-configmap"
)

type legacyModel struct {
	Model    cogitodevv1alpha1.LLMModelRef   `json:"model"`
	Artifact *cogitodevv1alpha1.ArtifactSpec `json:"artifact,omitempty"`
	Serving  cogitodevv1alpha1.ServingSpec   `json:"serving"`
}

// DecodeConfigMaps reads a multi-document Kubernetes YAML stream. It rejects
// non-ConfigMap documents so a migration command cannot silently omit input.
func DecodeConfigMaps(data []byte) ([]corev1.ConfigMap, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var configMaps []corev1.ConfigMap
	for {
		var configMap corev1.ConfigMap
		if err := decoder.Decode(&configMap); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode input: %w", err)
		}
		if configMap.APIVersion == "" && configMap.Kind == "" && configMap.Name == "" {
			continue
		}
		if configMap.APIVersion != "v1" || configMap.Kind != "ConfigMap" {
			return nil, fmt.Errorf("%s/%s is %s %s, want v1 ConfigMap", configMap.Namespace, configMap.Name, configMap.APIVersion, configMap.Kind)
		}
		configMaps = append(configMaps, configMap)
	}
	return configMaps, nil
}

// ConvertConfigMaps converts the labeled legacy ConfigMaps in deterministic
// namespace/name order. Unlabeled ConfigMaps are intentionally ignored.
func ConvertConfigMaps(configMaps []corev1.ConfigMap) ([]runtime.Object, error) {
	sort.Slice(configMaps, func(i, j int) bool {
		if configMaps[i].Namespace == configMaps[j].Namespace {
			return configMaps[i].Name < configMaps[j].Name
		}
		return configMaps[i].Namespace < configMaps[j].Namespace
	})

	objects := make([]runtime.Object, 0, len(configMaps))
	for _, configMap := range configMaps {
		isModel := configMap.Labels[ModelConfigLabel] == "true"
		isOverlay := configMap.Labels[ModelOverlayLabel] == "true"
		if !isModel && !isOverlay {
			continue
		}
		if isModel && isOverlay {
			return nil, fmt.Errorf("ConfigMap %s/%s has both migration labels", configMap.Namespace, configMap.Name)
		}
		if isModel {
			model, err := convertModel(configMap)
			if err != nil {
				return nil, err
			}
			objects = append(objects, model)
			continue
		}
		overlay, err := convertOverlay(configMap)
		if err != nil {
			return nil, err
		}
		objects = append(objects, overlay)
	}
	return objects, nil
}

func convertModel(configMap corev1.ConfigMap) (*cogitodevv1alpha1.LLMModel, error) {
	raw, ok := configMap.Data["model.yaml"]
	if !ok || raw == "" {
		return nil, fmt.Errorf("model ConfigMap %s/%s is missing data.model.yaml", configMap.Namespace, configMap.Name)
	}
	var legacy legacyModel
	if err := yaml.Unmarshal([]byte(raw), &legacy); err != nil {
		return nil, fmt.Errorf("decode model ConfigMap %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	if legacy.Model.Name == "" || legacy.Model.Source == "" || legacy.Serving.Backend == "" {
		return nil, fmt.Errorf("model ConfigMap %s/%s is missing model.name, model.source, or serving.backend", configMap.Namespace, configMap.Name)
	}
	return &cogitodevv1alpha1.LLMModel{
		TypeMeta:   metav1.TypeMeta{APIVersion: cogitodevv1alpha1.GroupVersion.String(), Kind: "LLMModel"},
		ObjectMeta: migratedObjectMeta(configMap, configMap.Name),
		Spec: cogitodevv1alpha1.LLMModelSpec{
			Model: legacy.Model, Artifact: legacy.Artifact, Serving: legacy.Serving,
		},
	}, nil
}

func convertOverlay(configMap corev1.ConfigMap) (*cogitodevv1alpha1.LLMModelOverlay, error) {
	name := configMap.Data["model_name"]
	displayName := configMap.Data["display_name"]
	baseModel := configMap.Data["base_model"]
	defaults := configMap.Data["request_defaults.json"]
	if name == "" || displayName == "" || baseModel == "" || defaults == "" {
		return nil, fmt.Errorf("overlay ConfigMap %s/%s is missing model_name, display_name, base_model, or request_defaults.json", configMap.Namespace, configMap.Name)
	}
	var jsonValue any
	if err := yaml.Unmarshal([]byte(defaults), &jsonValue); err != nil {
		return nil, fmt.Errorf("decode request defaults in overlay ConfigMap %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	jsonBytes, err := json.Marshal(jsonValue)
	if err != nil {
		return nil, fmt.Errorf("encode request defaults in overlay ConfigMap %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	return &cogitodevv1alpha1.LLMModelOverlay{
		TypeMeta:   metav1.TypeMeta{APIVersion: cogitodevv1alpha1.GroupVersion.String(), Kind: "LLMModelOverlay"},
		ObjectMeta: migratedObjectMeta(configMap, name),
		Spec: cogitodevv1alpha1.LLMModelOverlaySpec{
			DisplayName:     displayName,
			BaseModel:       baseModel,
			RequestDefaults: apiextensionsv1.JSON{Raw: jsonBytes},
		},
	}, nil
}

func migratedObjectMeta(configMap corev1.ConfigMap, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: configMap.Namespace,
		Annotations: map[string]string{
			MigratedFromAnnotation: configMap.Name,
		},
	}
}

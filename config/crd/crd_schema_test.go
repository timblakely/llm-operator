package crd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"sigs.k8s.io/yaml"
)

func TestCRDSchemasAreValid(t *testing.T) {
	files, err := filepath.Glob("llm.cogito.dev_*.yaml")
	if err != nil {
		t.Fatalf("find CRD manifests: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("expected 4 CRD manifests, found %d", len(files))
	}

	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) {
			external := readCRD(t, file)
			if len(external.Spec.Versions) != 1 || external.Spec.Versions[0].Name != "v1alpha1" {
				t.Fatalf("CRD API versions = %#v, want exactly v1alpha1", external.Spec.Versions)
			}
			internal := &apiextensions.CustomResourceDefinition{}
			if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(external, internal, nil); err != nil {
				t.Fatalf("convert CRD to internal API: %v", err)
			}
			for _, version := range internal.Spec.Versions {
				if version.Storage {
					internal.Status.StoredVersions = []string{version.Name}
					break
				}
			}
			if errs := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), internal); len(errs) != 0 {
				t.Fatalf("invalid CRD schema:\n%s", errs.ToAggregate())
			}
		})
	}
}

func TestBackendSchemasExposeSGLang(t *testing.T) {
	tests := []struct {
		file string
		path []string
	}{
		{
			file: "llm.cogito.dev_llmbackends.yaml",
			path: []string{"spec", "type"},
		},
		{
			file: "llm.cogito.dev_llmmodels.yaml",
			path: []string{"spec", "serving", "backend"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			schema := schemaAt(t, readCRD(t, tt.file), tt.path...)
			if !enumContains(schema, "sglang") {
				t.Fatalf("schema at %v does not allow sglang", tt.path)
			}
		})
	}
}

func TestModelSchemaExposesStructuredParsers(t *testing.T) {
	crd := readCRD(t, "llm.cogito.dev_llmmodels.yaml")
	for _, field := range []string{"toolCallParser", "reasoningParser"} {
		schema := schemaAt(t, crd, "spec", "serving", field)
		if schema.Type != "string" {
			t.Fatalf("spec.serving.%s has type %q, want string", field, schema.Type)
		}
	}
}

func TestOverlayBaseModelAcceptsCanonicalModelNames(t *testing.T) {
	schema := schemaAt(t, readCRD(t, "llm.cogito.dev_llmmodeloverlays.yaml"), "spec", "baseModel")
	pattern, err := regexp.Compile(schema.Pattern)
	if err != nil {
		t.Fatalf("compile baseModel pattern %q: %v", schema.Pattern, err)
	}
	if !pattern.MatchString("google/gemma-4-31B-it-qat-w4a16-ct") {
		t.Fatalf("baseModel pattern %q rejects a canonical model name", schema.Pattern)
	}
}

func readCRD(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	return crd
}

func schemaAt(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, path ...string) *apiextensionsv1.JSONSchemaProps {
	t.Helper()

	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatalf("CRD %s does not have exactly one version with an OpenAPI schema", crd.Name)
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, component := range path {
		next, ok := schema.Properties[component]
		if !ok {
			t.Fatalf("CRD %s schema is missing %s", crd.Name, fmt.Sprintf("%v", path))
		}
		schema = &next
	}
	return schema
}

func enumContains(schema *apiextensionsv1.JSONSchemaProps, expected string) bool {
	for _, value := range schema.Enum {
		if string(value.Raw) == fmt.Sprintf("%q", expected) {
			return true
		}
	}
	for i := range schema.AllOf {
		if enumContains(&schema.AllOf[i], expected) {
			return true
		}
	}
	return false
}

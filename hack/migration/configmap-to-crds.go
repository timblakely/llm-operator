// configmap-to-crds converts rendered legacy model ConfigMaps into reviewable
// LLM custom-resource YAML. It is intentionally file-based: render the target
// GitOps ConfigMaps first, review this output, then commit it to GitOps.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/timblakely/llm-operator/internal/migration"
)

func main() {
	input := flag.String("input", "", "multi-document ConfigMap YAML input (required)")
	output := flag.String("output", "", "output path (default stdout)")
	flag.Parse()
	if *input == "" {
		fmt.Fprintln(os.Stderr, "--input is required; render ConfigMaps from GitOps before converting")
		os.Exit(2)
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		fatal(err)
	}
	configMaps, err := migration.DecodeConfigMaps(data)
	if err != nil {
		fatal(err)
	}
	objects, err := migration.ConvertConfigMaps(configMaps)
	if err != nil {
		fatal(err)
	}
	var result []byte
	for i, object := range objects {
		encoded, err := marshalSpec(object)
		if err != nil {
			fatal(fmt.Errorf("encode output: %w", err))
		}
		if i > 0 {
			result = append(result, []byte("---\n")...)
		}
		result = append(result, encoded...)
	}
	if *output == "" {
		_, err = os.Stdout.Write(result)
	} else {
		err = os.WriteFile(*output, result, 0o644)
	}
	if err != nil {
		fatal(err)
	}
}

// marshalSpec strips status because migration output is declarative desired
// state; status is owned by the controller's status subresource.
func marshalSpec(object any) ([]byte, error) {
	data, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	delete(document, "status")
	return yaml.Marshal(document)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

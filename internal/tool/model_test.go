package tool

import (
	"context"
	"strings"
	"testing"
)

func TestFunctionSpecContract(t *testing.T) {
	spec := (Definition{Name: "send_to", Description: "handoff", Properties: map[string]any{"body": map[string]any{"type": "string"}}, Required: []string{"body"}}).FunctionSpec()
	if spec["type"] != "function" || spec["name"] != "send_to" {
		t.Fatalf("function spec = %+v", spec)
	}
	parameters, ok := spec["parameters"].(map[string]any)
	if !ok || parameters["additionalProperties"] != false {
		t.Fatalf("parameters = %+v", parameters)
	}
}

func TestRegistryRegistersExecutesAndListsDefinitions(t *testing.T) {
	registry := NewRegistry[string]()
	definition := Definition{Name: "echo", Description: "echo input"}
	if err := registry.Register(definition, func(_ context.Context, prefix string, args map[string]any) (string, error) {
		return prefix + args["value"].(string), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition, func(context.Context, string, map[string]any) (string, error) { return "", nil }); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate registration error = %v", err)
	}
	result, found, err := registry.Execute(context.Background(), "prefix:", "echo", map[string]any{"value": "hello"})
	if err != nil || !found || result != "prefix:hello" {
		t.Fatalf("execute = result %q, found %v, err %v", result, found, err)
	}
	if _, found, err := registry.Execute(context.Background(), "", "missing", nil); err != nil || found {
		t.Fatalf("missing execute = found %v, err %v", found, err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "echo" {
		t.Fatalf("definitions = %+v", definitions)
	}
}

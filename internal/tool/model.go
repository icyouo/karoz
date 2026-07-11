package tool

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type Call struct {
	ID        string
	CallID    string
	Name      string
	Arguments string
}

type Definition struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

func (definition Definition) FunctionSpec() map[string]any {
	properties := definition.Properties
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(definition.Required) > 0 {
		schema["required"] = append([]string{}, definition.Required...)
	}
	return map[string]any{
		"type": "function", "name": definition.Name, "description": definition.Description, "parameters": schema,
	}
}

type Handler[C any] func(context.Context, C, map[string]any) (string, error)

type Registration[C any] struct {
	Definition Definition
	Handler    Handler[C]
}

type Registry[C any] struct {
	mu            sync.RWMutex
	registrations map[string]Registration[C]
}

func NewRegistry[C any]() *Registry[C] {
	return &Registry[C]{registrations: map[string]Registration[C]{}}
}

func (registry *Registry[C]) Register(definition Definition, handler Handler[C]) error {
	if registry == nil {
		return fmt.Errorf("tool registry is nil")
	}
	if definition.Name == "" {
		return fmt.Errorf("tool name is required")
	}
	if handler == nil {
		return fmt.Errorf("tool %q handler is required", definition.Name)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.registrations == nil {
		registry.registrations = map[string]Registration[C]{}
	}
	if _, exists := registry.registrations[definition.Name]; exists {
		return fmt.Errorf("tool %q is already registered", definition.Name)
	}
	registry.registrations[definition.Name] = Registration[C]{Definition: definition, Handler: handler}
	return nil
}

func (registry *Registry[C]) Execute(ctx context.Context, executionContext C, name string, args map[string]any) (string, bool, error) {
	if registry == nil {
		return "", false, nil
	}
	registry.mu.RLock()
	registration, exists := registry.registrations[name]
	registry.mu.RUnlock()
	if !exists {
		return "", false, nil
	}
	result, err := registration.Handler(ctx, executionContext, args)
	return result, true, err
}

func (registry *Registry[C]) Definitions() []Definition {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	definitions := make([]Definition, 0, len(registry.registrations))
	for _, registration := range registry.registrations {
		definitions = append(definitions, registration.Definition)
	}
	registry.mu.RUnlock()
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions
}

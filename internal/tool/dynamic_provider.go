package tool

import "context"

type DynamicProvider interface {
	Specs(context.Context, string) []map[string]any
	Call(context.Context, string, string, string) (string, error)
}

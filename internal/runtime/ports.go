package runtime

import "context"

type ModelProvider[Request, ExecutionContext, Callbacks any] interface {
	Stream(context.Context, Request, ExecutionContext, Callbacks) error
}

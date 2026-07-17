package runtime

import "context"

type ProviderCapabilities struct {
	Streaming  bool
	Tools      bool
	Interrupts bool
}

func (capabilities ProviderCapabilities) SupportsResidentRuntime() bool {
	return capabilities.Streaming && capabilities.Tools && capabilities.Interrupts
}

type ModelProvider[Request, ExecutionContext, Callbacks any] interface {
	Capabilities(Request) ProviderCapabilities
	Stream(context.Context, Request, ExecutionContext, Callbacks) error
}

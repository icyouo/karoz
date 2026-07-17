package main

import (
	"context"
	runtimedomain "github.com/karoz/karoz/internal/runtime"
	tooldomain "github.com/karoz/karoz/internal/tool"
)

type cliModelProviderAdapter struct {
	app *app
}

func (adapter cliModelProviderAdapter) Capabilities(request CLI2APIRequest) runtimedomain.ProviderCapabilities {
	return adapter.app.residentProviderCapabilities(request.Provider)
}

func (adapter cliModelProviderAdapter) Stream(ctx context.Context, request CLI2APIRequest, toolCtx ResidentToolContext, callbacks AgentStreamCallbacks) error {
	return adapter.app.invokeCLI2APIStream(ctx, request, toolCtx, callbacks)
}

type mcpDynamicToolAdapter struct {
	app *app
}

func (adapter mcpDynamicToolAdapter) Specs(ctx context.Context, workdir string) []map[string]any {
	return adapter.app.mcpToolSpecs(ctx, workdir)
}

func (adapter mcpDynamicToolAdapter) Call(ctx context.Context, workdir, name, arguments string) (string, error) {
	return adapter.app.callMCPTool(ctx, workdir, name, arguments)
}

func (a *app) residentModelProvider() runtimedomain.ModelProvider[CLI2APIRequest, ResidentToolContext, AgentStreamCallbacks] {
	if a.modelProvider != nil {
		return a.modelProvider
	}
	return cliModelProviderAdapter{app: a}
}

func (a *app) dynamicToolProvider() tooldomain.DynamicProvider {
	if a.dynamicTools != nil {
		return a.dynamicTools
	}
	return mcpDynamicToolAdapter{app: a}
}

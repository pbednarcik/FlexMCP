package server

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

type PromptCompletionProvider interface {
	// CompletePromptArgument provides completions for a prompt argument
	CompletePromptArgument(ctx context.Context, promptName string, argument mcp.CompleteArgument, context mcp.CompleteContext) (*mcp.Completion, error)
}

type ResourceCompletionProvider interface {
	// CompleteResourceArgument provides completions for a resource template argument
	CompleteResourceArgument(ctx context.Context, uri string, argument mcp.CompleteArgument, context mcp.CompleteContext) (*mcp.Completion, error)
}

// DefaultCompletionProvider returns no completions (fallback)
type DefaultPromptCompletionProvider struct{}

func (p *DefaultPromptCompletionProvider) CompletePromptArgument(ctx context.Context, promptName string, argument mcp.CompleteArgument, context mcp.CompleteContext) (*mcp.Completion, error) {
	return &mcp.Completion{
		Values: []string{},
	}, nil
}

// DefaultResourceCompletionProvider returns no completions (fallback)
type DefaultResourceCompletionProvider struct{}

func (p *DefaultResourceCompletionProvider) CompleteResourceArgument(ctx context.Context, uri string, argument mcp.CompleteArgument, context mcp.CompleteContext) (*mcp.Completion, error) {
	return &mcp.Completion{
		Values: []string{},
	}, nil
}

func (s *MCPServer) handleComplete(
	ctx context.Context,
	id any,
	request mcp.CompleteRequest,
) (*mcp.CompleteResult, *requestError) {
	var completion *mcp.Completion
	var err error
	switch ref := request.Params.Ref.(type) {
	case mcp.PromptReference:
		completion, err = s.promptCompletionProvider.CompletePromptArgument(
			ctx,
			ref.Name,
			request.Params.Argument,
			request.Params.Context,
		)
	case mcp.ResourceReference:
		completion, err = s.resourceCompletionProvider.CompleteResourceArgument(
			ctx,
			ref.URI,
			request.Params.Argument,
			request.Params.Context,
		)
	default:
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_REQUEST,
			err:  fmt.Errorf("unknown reference type: %v", ref),
		}
	}
	if err != nil {
		return nil, handlerError(ctx, id, err)
	}

	// Defensive nil check: default providers always return non-nil completions,
	// but custom providers might erroneously return nil. Treat as empty result.
	if completion == nil {
		completion = &mcp.Completion{}
	}

	result := &mcp.CompleteResult{
		Completion: *completion,
	}
	// values is a required array, so a provider with no matches still sends []
	// rather than null.
	if result.Completion.Values == nil {
		result.Completion.Values = []string{}
	}
	return result, nil
}

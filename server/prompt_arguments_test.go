package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestGetPromptRequiresDeclaredArguments pins Prompts.md row 10: a prompts/get
// that omits an argument the prompt declared required is answered -32602
// before the handler runs; an optional argument may be omitted.
func TestGetPromptRequiresDeclaredArguments(t *testing.T) {
	srv := NewMCPServer("prompts", "1.0.0", WithPromptCapabilities(false))
	handled := 0
	srv.AddPrompt(mcp.NewPrompt("greet",
		mcp.WithArgument("who", mcp.RequiredArgument()),
		mcp.WithArgument("tone"),
	), func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		handled++
		return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{
			mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("hello "+request.Params.Arguments["who"])),
		}}, nil
	})

	get := func(arguments map[string]string) (*mcp.GetPromptResult, *requestError) {
		return srv.handleGetPrompt(t.Context(), 1, mcp.GetPromptRequest{Params: mcp.GetPromptParams{Name: "greet", Arguments: arguments}})
	}

	result, reqErr := get(map[string]string{"who": "world"})
	require.Nil(t, reqErr)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, 1, handled, "the optional argument may be omitted")

	for name, arguments := range map[string]map[string]string{"no arguments": nil, "only the optional one": {"tone": "warm"}} {
		result, reqErr := get(arguments)
		assert.Nil(t, result, name)
		require.NotNil(t, reqErr, name)
		assert.Equal(t, mcp.INVALID_PARAMS, reqErr.code, name)
		assert.ErrorContains(t, reqErr, `missing required argument "who"`, name)
	}
	assert.Equal(t, 1, handled, "the handler never ran without the required argument")
}

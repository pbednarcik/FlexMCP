package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A resource template without a URI template is refused before anything
// changes: the batch's other templates are not registered, the server does
// not advertise resources it does not have, and the resources lock is free,
// so the next call is answered.
func TestAResourceTemplateWithoutAURITemplateIsRefused(t *testing.T) {
	handler := func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) { return nil, nil }
	good := ServerResourceTemplate{Template: mcp.NewResourceTemplate("file:///{name}", "good"), Handler: handler}
	for name, refused := range map[string]mcp.ResourceTemplate{
		"no URITemplate":       {Name: "refused"},
		"an empty URITemplate": {Name: "refused", URITemplate: &mcp.URITemplate{}},
	} {
		for verb, register := range map[string]func(*MCPServer){
			"AddResourceTemplates": func(s *MCPServer) {
				s.AddResourceTemplates(good, ServerResourceTemplate{Template: refused, Handler: handler})
			},
			"SetResourceTemplates": func(s *MCPServer) {
				s.SetResourceTemplates(good, ServerResourceTemplate{Template: refused, Handler: handler})
			},
			"AddResourceTemplate": func(s *MCPServer) { s.AddResourceTemplate(refused, handler) },
		} {
			t.Run(name+"/"+verb, func(t *testing.T) {
				srv := NewMCPServer("p", "1.0.0")
				require.PanicsWithValue(t, `resource template "refused" has no URI template`, func() { register(srv) })
				assert.Nil(t, initializedCapabilities(t, srv).Resources, "no resources advertised")
				assert.Empty(t, srv.resourceTemplates, "no template of the batch is registered")

				answered := make(chan mcp.JSONRPCMessage, 1)
				go func() {
					answered <- srv.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"resources/templates/list"}`))
				}()
				select {
				case answer := <-answered:
					listErr, ok := answer.(mcp.JSONRPCError)
					require.True(t, ok, "resources are not served: %#v", answer)
					assert.Equal(t, mcp.METHOD_NOT_FOUND, listErr.Error.Code)
				case <-time.After(5 * time.Second):
					t.Fatal("resources/templates/list was not answered: the resources lock is held")
				}

				srv.AddResourceTemplates(good)
				require.NotNil(t, initializedCapabilities(t, srv).Resources, "the first template registered turns resources on")
			})
		}
	}
}

// A refused SetResourceTemplates keeps the templates registered before it:
// the call changes nothing.
func TestARefusedSetResourceTemplatesKeepsTheTemplates(t *testing.T) {
	handler := func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) { return nil, nil }
	srv := NewMCPServer("p", "1.0.0")
	srv.AddResourceTemplate(mcp.NewResourceTemplate("file:///{name}", "kept"), handler)
	require.Panics(t, func() {
		srv.SetResourceTemplates(
			ServerResourceTemplate{Template: mcp.NewResourceTemplate("file:///new/{name}", "new"), Handler: handler},
			ServerResourceTemplate{Template: mcp.ResourceTemplate{Name: "refused"}, Handler: handler},
		)
	})
	require.Len(t, srv.resourceTemplates, 1)
	assert.Contains(t, srv.resourceTemplates, "file:///{name}")
}

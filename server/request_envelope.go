package server

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

// jsonrpcEnvelope is the JSON-RPC members of a message.
type jsonrpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  mcp.MCPMethod   `json:"method"`
	RawID   json.RawMessage `json:"id,omitempty"`
	ID      any             `json:"-"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// requestEnvelope is a message read once for every check before dispatch:
// its JSON-RPC members and what those checks read from params.
type requestEnvelope struct {
	jsonrpcEnvelope
	Params paramsPeek `json:"params"`
}

// paramsPeek is what the checks before dispatch read from params: the
// protocol keys of _meta, and the name or uri the Mcp-Name header carries.
type paramsPeek struct {
	Meta requestMeta `json:"_meta"`
	Name string      `json:"name"`
	URI  string      `json:"uri"`
}

// headerName reports what [mcp.ExtractHeaderName] finds in params for
// method: the name of a tools/call or prompts/get, the uri of a
// resources/read.
func (p *paramsPeek) headerName(method mcp.MCPMethod) (string, bool) {
	switch method {
	case mcp.MethodToolsCall, mcp.MethodPromptsGet:
		return p.Name, true
	case mcp.MethodResourcesRead:
		return p.URI, true
	}
	return "", false
}

// requestReader is a session whose transport read the request it serves
// before dispatch.
type requestReader interface {
	readRequest(message json.RawMessage) *requestEnvelope
}

// readByTransport is the transport's read of message, when the request's
// session kept one; nil otherwise.
func readByTransport(ctx context.Context, message json.RawMessage) *requestEnvelope {
	if reader, ok := ClientSessionFromContext(ctx).(requestReader); ok {
		return reader.readRequest(message)
	}
	return nil
}

// hooksHoldTheMessage reports whether a hook receives a request's message
// itself before the checks that read it, and so could change it.
func hooksHoldTheMessage(hooks *Hooks) bool {
	return hooks != nil && len(hooks.OnRequestInitialization) > 0
}

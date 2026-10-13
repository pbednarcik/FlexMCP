package server

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"

	"github.com/mark3labs/mcp-go/mcp"
)

// readEnvelope reads a message once, by encoding/json's own rules except that
// a member named twice anywhere fails the read. Read into types, a duplicate
// would merge into or keep the earlier value where the per-check reads kept
// the last, so such a message takes those reads instead.
func readEnvelope(message []byte, envelope *requestEnvelope) error {
	return jsonv2.Unmarshal(message, envelope, envelopeOptions)
}

var envelopeOptions = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsontext.AllowDuplicateNames(false))

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
// protocol keys of _meta, the name or uri the Mcp-Name header carries, and
// the request a cancellation names, which the stdio transport acts on.
type paramsPeek struct {
	Meta      requestMeta     `json:"_meta"`
	Name      string          `json:"name"`
	URI       string          `json:"uri"`
	RequestID json.RawMessage `json:"requestId"`
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

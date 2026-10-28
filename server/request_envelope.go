package server

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"

	"github.com/pbednarcik/flexmcp/mcp"
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
// protocol keys of _meta, the name, uri or task id the Mcp-Name header
// carries, and the request a cancellation names, which the stdio transport
// acts on.
type paramsPeek struct {
	Meta requestMeta `json:"_meta"`
	Name string      `json:"name"`
	URI  string      `json:"uri"`
	// TaskId is a pointer, so the envelope of any other request stays small.
	TaskId    *taskIDPeek     `json:"taskId"`
	RequestID json.RawMessage `json:"requestId"`
}

// taskIDPeek is a taskId as the Mcp-Name check reads it: one of another type
// fails that check, as in [mcp.ExtractHeaderName], and not the read.
type taskIDPeek struct {
	id       string
	isString bool
}

func (p *taskIDPeek) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	value, err := dec.ReadValue()
	if err != nil {
		return err
	}
	p.isString = json.Unmarshal(value, &p.id) == nil
	return nil
}

// headerName reports what [mcp.ExtractHeaderName] finds in params for
// method: the name of a tools/call or prompts/get, the uri of a
// resources/read, the task id of a tasks/get, tasks/cancel or tasks/update.
func (p *paramsPeek) headerName(method mcp.MCPMethod) (string, bool) {
	switch method {
	case mcp.MethodToolsCall, mcp.MethodPromptsGet:
		return p.Name, true
	case mcp.MethodResourcesRead:
		return p.URI, true
	case mcp.MethodTasksGet, mcp.MethodTasksCancel, mcp.MethodTasksUpdate:
		if p.TaskId == nil {
			return "", true
		}
		return p.TaskId.id, p.TaskId.isString
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

// requestIDValue decodes a JSON-RPC id from the wire as mcp.RequestId does: a
// string, or an integer kept exact, so an id past 2^53 is echoed and matched
// as sent. An absent id is nil, a notification's. ok is false for any other
// id, null, a fraction, a boolean, an object, an array or a number no type
// holds such as 1e400: a request id is a string or an integer and never null
// (MCP base protocol), and such a request is invalid, never run or answered
// as some other id.
func requestIDValue(raw json.RawMessage) (id any, ok bool) {
	if len(raw) == 0 {
		return nil, true
	}
	// raw is a value as a decoder gave it, so it is read by the id's own
	// UnmarshalJSON, called directly: through json.Unmarshal the id would be
	// made on the heap.
	var requestID mcp.RequestId
	if requestID.UnmarshalJSON(raw) != nil {
		return nil, false
	}
	switch value := requestID.Value(); value.(type) {
	case string, int64, json.Number:
		return value, true
	}
	return nil, false
}

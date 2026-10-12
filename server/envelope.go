package server

import (
	"bytes"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
)

// requestEnvelope is a JSON-RPC message decoded once, for every check and
// dispatch decision made before the typed decode of the request, where each
// of them used to decode the whole body again.
//
// Each value is kept raw and decoded on its own where it is read, so a value
// of the wrong shape fails only the check that reads it, as it did when that
// check decoded the body itself. method is typed, as every reader decoded it
// typed: a method that is not a string fails the envelope.
type requestEnvelope struct {
	JSONRPC json.RawMessage `json:"jsonrpc"`
	Method  mcp.MCPMethod   `json:"method"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
	Params  json.RawMessage `json:"params"`

	// What the accessors decoded, kept so each is decoded once.
	decodedID     any
	idRead        bool
	params        paramsView
	paramsRead    bool
	paramsOK      bool
	wellKnown     wellKnownMeta
	wellKnownRead bool
	wellKnownOK   bool
}

// paramsView is params decoded once, its values raw: _meta, and the name a
// tools/call or prompts/get names.
type paramsView struct {
	Meta json.RawMessage `json:"_meta"`
	Name json.RawMessage `json:"name"`
}

// wellKnownMeta is _meta's well-known keys, raw; the tags spell
// mcp.MetaKeyProtocolVersion, MetaKeyClientInfo, MetaKeyClientCapabilities
// and MetaKeyLogLevel.
type wellKnownMeta struct {
	ProtocolVersion    json.RawMessage `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         json.RawMessage `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
	LogLevel           json.RawMessage `json:"io.modelcontextprotocol/logLevel"`
}

// decodeEnvelope decodes message's envelope, reporting whether it is a JSON
// object whose method, if any, is a string.
func decodeEnvelope(message []byte) (*requestEnvelope, bool) {
	var envelope requestEnvelope
	if json.Unmarshal(message, &envelope) != nil {
		return nil, false
	}
	return &envelope, true
}

// id returns the id decoded as an untyped value, nil when absent or null.
func (e *requestEnvelope) id() any {
	if !e.idRead {
		e.idRead = true
		if len(e.ID) > 0 {
			_ = json.Unmarshal(e.ID, &e.decodedID)
		}
	}
	return e.decodedID
}

// jsonrpcVersion returns the jsonrpc member, "" when absent or null, and
// false when it is not a string.
func (e *requestEnvelope) jsonrpcVersion() (string, bool) {
	var version string
	if len(e.JSONRPC) > 0 && json.Unmarshal(e.JSONRPC, &version) != nil {
		return "", false
	}
	return version, true
}

// isResponse reports whether the message carries a result or an error that
// is not null: a response to a request the server sent.
func (e *requestEnvelope) isResponse() bool {
	return !isNullOrAbsent(e.Result) || !isNullOrAbsent(e.Error)
}

// view returns params decoded once, and false when params is absent or not
// an object.
func (e *requestEnvelope) view() (*paramsView, bool) {
	if !e.paramsRead {
		e.paramsRead = true
		e.paramsOK = len(e.Params) > 0 && json.Unmarshal(e.Params, &e.params) == nil
	}
	return &e.params, e.paramsOK
}

// meta returns _meta's well-known keys, and false when params or _meta is not
// an object.
func (e *requestEnvelope) meta() (wellKnownMeta, bool) {
	if !e.wellKnownRead {
		e.wellKnownRead = true
		view, ok := e.view()
		e.wellKnownOK = ok && (len(view.Meta) == 0 || json.Unmarshal(view.Meta, &e.wellKnown) == nil)
	}
	return e.wellKnown, e.wellKnownOK
}

// metaProtocolVersion returns the protocol version _meta declares, "" when it
// declares none or not as a string.
func (e *requestEnvelope) metaProtocolVersion() string {
	meta, ok := e.meta()
	if !ok || len(meta.ProtocolVersion) == 0 {
		return ""
	}
	var version string
	if json.Unmarshal(meta.ProtocolVersion, &version) != nil {
		return ""
	}
	return version
}

// paramsName returns the name params gives, and false when params or the
// name is not of the shape a name is read from.
func (e *requestEnvelope) paramsName() (string, bool) {
	view, ok := e.view()
	if !ok {
		return "", false
	}
	var name string
	if len(view.Name) > 0 && json.Unmarshal(view.Name, &name) != nil {
		return "", false
	}
	return name, true
}

// isNullOrAbsent reports whether raw is no value at all, or JSON null.
func isNullOrAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

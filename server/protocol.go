package server

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pbednarcik/flexmcp/mcp"
)

// RequestProtocolInfo describes the protocol era and per-request metadata of
// an incoming request.
//
// Protocol version 2026-07-28 removed the initialize handshake: every request
// carries its own protocol version, client identity, client capabilities, and
// desired log level in _meta (SEP-2575). This type normalizes that
// information so the rest of the server can treat both eras uniformly.
type RequestProtocolInfo struct {
	// Modern reports whether the request declared a protocol version of
	// 2026-07-28 or later in its _meta.
	Modern bool

	// ProtocolVersion is the version declared in _meta. It is empty for legacy
	// requests, whose version was fixed by the initialize handshake.
	ProtocolVersion string

	// ClientInfo identifies the client software, when it chose to say.
	ClientInfo *mcp.Implementation

	// ClientCapabilities are the capabilities the client declared for this
	// request. Servers MUST NOT infer capabilities from prior requests.
	ClientCapabilities *mcp.ClientCapabilities

	// LogLevel is the log level requested for this request. When empty, the
	// server MUST NOT emit notifications/message for the request.
	LogLevel mcp.LoggingLevel
}

type requestProtocolKey struct{}

// WithRequestProtocolInfo returns a context carrying the per-request protocol
// information.
func WithRequestProtocolInfo(ctx context.Context, info *RequestProtocolInfo) context.Context {
	return context.WithValue(ctx, requestProtocolKey{}, info)
}

// RequestProtocolInfoFromContext returns the per-request protocol information
// stored in ctx, or nil when the request did not carry any (that is, when it
// used a protocol version earlier than 2026-07-28).
func RequestProtocolInfoFromContext(ctx context.Context) *RequestProtocolInfo {
	info, _ := ctx.Value(requestProtocolKey{}).(*RequestProtocolInfo)
	return info
}

// IsModernRequest reports whether the request being handled uses the
// stateless protocol core introduced in 2026-07-28.
func IsModernRequest(ctx context.Context) bool {
	info := RequestProtocolInfoFromContext(ctx)
	return info != nil && info.Modern
}

// ClientDeclaresExtension reports whether the request being handled declared
// the extension id under capabilities.extensions in its _meta. Capabilities
// are declared per request, so this is the answer for this request alone. A
// legacy request never declares one: extensions arrived with protocol version
// 2026-07-28, and the initialize handshake is not consulted.
func ClientDeclaresExtension(ctx context.Context, id string) bool {
	info := RequestProtocolInfoFromContext(ctx)
	if info == nil || !info.Modern || info.ClientCapabilities == nil {
		return false
	}
	_, ok := info.ClientCapabilities.Extensions[id]
	return ok
}

// RequestProtocolVersion returns the protocol version in effect for the
// request being handled.
//
// For modern requests this is the version declared in _meta. For legacy
// requests it falls back to the version negotiated during initialize, when the
// session records one.
func RequestProtocolVersion(ctx context.Context) string {
	if info := RequestProtocolInfoFromContext(ctx); info != nil && info.ProtocolVersion != "" {
		return info.ProtocolVersion
	}
	if session := ClientSessionFromContext(ctx); session != nil {
		if versioned, ok := session.(SessionWithProtocolVersion); ok {
			return versioned.ProtocolVersion()
		}
	}
	return ""
}

// SessionWithProtocolVersion is implemented by sessions that remember the
// protocol version negotiated during initialize.
type SessionWithProtocolVersion interface {
	ClientSession
	// ProtocolVersion returns the negotiated protocol version, or "" when the
	// session has not been initialized.
	ProtocolVersion() string
}

// extractRequestProtocolInfo performs a lightweight partial unmarshal of the
// _meta field of a JSON-RPC request and reports what protocol era it belongs
// to.
//
// A request belongs to the modern era only when it declares a protocol version
// of 2026-07-28 or later. Anything else - including a request with no _meta at
// all - is legacy; both eras are served by the same handlers.
//
// The returned error is non-nil only when a modern request is malformed: the
// required client capabilities are missing, or a well-known key holds a value
// of the wrong shape.
func extractRequestProtocolInfo(message json.RawMessage) (*RequestProtocolInfo, error) {
	// Each key stays raw and is decoded on its own, so a malformed optional
	// one is ignored.
	var wrapper struct {
		Params struct {
			Meta struct {
				ProtocolVersion    json.RawMessage `json:"io.modelcontextprotocol/protocolVersion"`
				ClientInfo         json.RawMessage `json:"io.modelcontextprotocol/clientInfo"`
				ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
				LogLevel           json.RawMessage `json:"io.modelcontextprotocol/logLevel"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(message, &wrapper); err != nil {
		// A malformed body is reported by the per-method unmarshal, which
		// produces a better error message.
		return &RequestProtocolInfo{}, nil
	}
	raw := wrapper.Params.Meta
	var meta requestMeta
	_ = json.Unmarshal(raw.ProtocolVersion, &meta.ProtocolVersion)
	if len(raw.ClientInfo) > 0 {
		var clientInfo mcp.Implementation
		if json.Unmarshal(raw.ClientInfo, &clientInfo) == nil {
			meta.ClientInfo = metaClientInfo{info: &clientInfo}
		}
	}
	if len(raw.ClientCapabilities) > 0 && string(raw.ClientCapabilities) != "null" {
		var caps mcp.ClientCapabilities
		if json.Unmarshal(raw.ClientCapabilities, &caps) == nil {
			meta.ClientCapabilities = &caps
		}
	}
	meta.LogLevel = raw.LogLevel
	return protocolInfoFromMeta(meta)
}

// requestMeta is the four well-known _meta keys a request's protocol info is
// read from, typed, for a request read once: a value of the wrong type fails
// that read, and [extractRequestProtocolInfo] then reads each key on its own.
// The tags spell mcp.MetaKeyProtocolVersion, MetaKeyClientInfo,
// MetaKeyClientCapabilities and MetaKeyLogLevel.
type requestMeta struct {
	ProtocolVersion    string                  `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         metaClientInfo          `json:"io.modelcontextprotocol/clientInfo"`
	ClientCapabilities *mcp.ClientCapabilities `json:"io.modelcontextprotocol/clientCapabilities"`
	// Raw: few requests carry it, and null is a log level that is not one
	// of the eight.
	LogLevel json.RawMessage `json:"io.modelcontextprotocol/logLevel"`
}

// metaClientInfo is a client identity, set once it was sent, so that a null
// one still counts as sent. A pointer keeps a request that sends none from
// carrying room for one.
type metaClientInfo struct {
	info *mcp.Implementation
}

func (c *metaClientInfo) UnmarshalJSON(data []byte) error {
	c.info = new(mcp.Implementation)
	return json.Unmarshal(data, c.info)
}

// protocolInfoFromMeta reports the era and protocol info of a request from
// its _meta keys.
func protocolInfoFromMeta(meta requestMeta) (*RequestProtocolInfo, error) {
	info := new(RequestProtocolInfo)
	if err := readProtocolInfo(meta, info); err != nil {
		return nil, err
	}
	return info, nil
}

// readProtocolInfo reads the era and protocol info of a request from its
// _meta keys into info.
func readProtocolInfo(meta requestMeta, info *RequestProtocolInfo) error {
	version := meta.ProtocolVersion
	if !mcp.IsModernProtocol(version) {
		// Either no version at all, or an older one: legacy era.
		*info = RequestProtocolInfo{ProtocolVersion: version}
		return nil
	}

	*info = RequestProtocolInfo{
		Modern:          true,
		ProtocolVersion: version,
	}
	info.ClientInfo = meta.ClientInfo.info
	// A log level, when given, is one of the eight; anything else is an
	// invalid params error (Utilities.md row 11), not a silent no-op.
	if len(meta.LogLevel) > 0 {
		if json.Unmarshal(meta.LogLevel, &info.LogLevel) != nil || !info.LogLevel.IsValid() {
			return errors.New("invalid _meta field " + mcp.MetaKeyLogLevel)
		}
	}

	// Client capabilities are required on every modern request. An empty
	// object is valid and means "no optional capabilities". A request without
	// them, or with a value that does not decode as capabilities, is malformed
	// and is rejected as invalid params (-32602).
	// MissingRequiredClientCapabilityError is for a request that needs a
	// capability the client did not declare.
	if meta.ClientCapabilities == nil {
		return errors.New("missing or invalid _meta field " + mcp.MetaKeyClientCapabilities)
	}
	info.ClientCapabilities = meta.ClientCapabilities

	return nil
}

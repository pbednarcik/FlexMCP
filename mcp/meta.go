package mcp

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"maps"
	"slices"
	"strconv"
)

// Well-known _meta keys defined by protocol version 2026-07-28 (SEP-2575).
//
// These keys carry the information that the initialize handshake used to
// establish once per session. They are namespaced with the reserved
// "io.modelcontextprotocol/" prefix.
const (
	// MetaKeyProtocolVersion identifies the MCP protocol version a request is
	// using. It is required on every request in protocol versions >= 2026-07-28
	// and, over HTTP, MUST match the MCP-Protocol-Version header.
	MetaKeyProtocolVersion = "io.modelcontextprotocol/protocolVersion"

	// MetaKeyClientInfo identifies the client software making a request.
	// Clients SHOULD include it on every request. The value is an
	// [Implementation]. It is self-reported and unverified: servers SHOULD NOT
	// use it for security decisions.
	MetaKeyClientInfo = "io.modelcontextprotocol/clientInfo"

	// MetaKeyClientCapabilities carries the client's capabilities for a single
	// request. It is required in protocol versions >= 2026-07-28. Servers MUST
	// NOT infer capabilities from prior requests. The value is a
	// [ClientCapabilities].
	MetaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"

	// MetaKeyServerInfo identifies the server software producing a result.
	// Servers SHOULD include it in every result's _meta. The value is an
	// [Implementation].
	MetaKeyServerInfo = "io.modelcontextprotocol/serverInfo"

	// MetaKeyLogLevel requests notifications/message at or above the given
	// level for a single request. When absent, servers MUST NOT emit log
	// notifications for that request. Replaces the logging/setLevel RPC.
	//
	// The Logging feature it belongs to is deprecated as of protocol version
	// 2026-07-28 (SEP-2577), and remains functional for at least twelve
	// months. Log to stderr or use OpenTelemetry instead.
	MetaKeyLogLevel = "io.modelcontextprotocol/logLevel"

	// MetaKeySubscriptionID identifies the subscriptions/listen stream a
	// notification was delivered on. The value is the JSON-RPC ID of the
	// subscriptions/listen request that opened the stream.
	MetaKeySubscriptionID = "io.modelcontextprotocol/subscriptionId"
)

// remarshal round-trips src through JSON into dst. It is used to convert
// generic map[string]any values decoded from the wire into concrete types.
func remarshal(src any, dst any) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

// decodeMetaValue extracts a typed value from a _meta map.
//
// Values may arrive either as a concrete Go value (when constructed in
// process, e.g. over the in-process transport) or as the generic
// map[string]any produced by encoding/json after wire transit. In the latter
// case the value is re-encoded and decoded into the target type.
func decodeMetaValue[T any](m map[string]any, key string) (T, bool) {
	var zero T
	raw, ok := m[key]
	if !ok || raw == nil {
		return zero, false
	}
	if typed, ok := raw.(T); ok {
		return typed, true
	}
	// Also accept a pointer when T is a value type and vice versa.
	var out T
	if err := remarshal(raw, &out); err != nil {
		return zero, false
	}
	return out, true
}

// GetMetaField returns the raw value stored under key in m's additional
// fields, or nil when absent.
func (m *Meta) GetMetaField(key string) any {
	if m == nil || m.AdditionalFields == nil {
		return nil
	}
	return m.AdditionalFields[key]
}

// SetMetaField stores value under key in m's additional fields.
func (m *Meta) SetMetaField(key string, value any) {
	if m.AdditionalFields == nil {
		m.AdditionalFields = make(map[string]any)
	}
	m.AdditionalFields[key] = value
}

// ProtocolVersion returns the protocol version declared in the _meta field, or
// "" when absent. See [MetaKeyProtocolVersion].
func (m *Meta) ProtocolVersion() string {
	if m == nil {
		return ""
	}
	version, _ := decodeMetaValue[string](m.AdditionalFields, MetaKeyProtocolVersion)
	return version
}

// SetProtocolVersion records the protocol version in the _meta field.
func (m *Meta) SetProtocolVersion(version string) {
	m.SetMetaField(MetaKeyProtocolVersion, version)
}

// ClientInfo returns the client implementation declared in the _meta field, or
// nil when absent or malformed. See [MetaKeyClientInfo].
func (m *Meta) ClientInfo() *Implementation {
	if m == nil {
		return nil
	}
	info, ok := decodeMetaValue[*Implementation](m.AdditionalFields, MetaKeyClientInfo)
	if !ok {
		return nil
	}
	return info
}

// SetClientInfo records the client implementation in the _meta field.
func (m *Meta) SetClientInfo(info Implementation) {
	m.SetMetaField(MetaKeyClientInfo, info)
}

// ClientCapabilities returns the per-request client capabilities declared in
// the _meta field, or nil when absent or malformed.
// See [MetaKeyClientCapabilities].
func (m *Meta) ClientCapabilities() *ClientCapabilities {
	if m == nil {
		return nil
	}
	caps, ok := decodeMetaValue[*ClientCapabilities](m.AdditionalFields, MetaKeyClientCapabilities)
	if !ok {
		return nil
	}
	return caps
}

// SetClientCapabilities records the per-request client capabilities in the
// _meta field.
func (m *Meta) SetClientCapabilities(caps ClientCapabilities) {
	m.SetMetaField(MetaKeyClientCapabilities, caps)
}

// ServerInfo returns the server implementation declared in the _meta field, or
// nil when absent or malformed. See [MetaKeyServerInfo].
func (m *Meta) ServerInfo() *Implementation {
	if m == nil {
		return nil
	}
	info, ok := decodeMetaValue[*Implementation](m.AdditionalFields, MetaKeyServerInfo)
	if !ok {
		return nil
	}
	return info
}

// SetServerInfo records the server implementation in the _meta field.
func (m *Meta) SetServerInfo(info Implementation) {
	m.SetMetaField(MetaKeyServerInfo, info)
}

// LogLevel returns the per-request log level declared in the _meta field, or
// "" when absent. See [MetaKeyLogLevel].
func (m *Meta) LogLevel() LoggingLevel {
	if m == nil {
		return ""
	}
	level, _ := decodeMetaValue[LoggingLevel](m.AdditionalFields, MetaKeyLogLevel)
	return level
}

// SetLogLevel records the per-request log level in the _meta field.
func (m *Meta) SetLogLevel(level LoggingLevel) {
	m.SetMetaField(MetaKeyLogLevel, level)
}

// SubscriptionID returns the subscription stream identifier declared in the
// _meta field, or nil when absent. See [MetaKeySubscriptionID].
func (m *Meta) SubscriptionID() any {
	if m == nil {
		return nil
	}
	return m.GetMetaField(MetaKeySubscriptionID)
}

// SetSubscriptionID records the subscription stream identifier in the _meta
// field.
func (m *Meta) SetSubscriptionID(id any) {
	m.SetMetaField(MetaKeySubscriptionID, id)
}

// Meta is metadata attached to a request's parameters. This can include fields
// formally defined by the protocol or other arbitrary data.
type Meta struct {
	// If specified, the caller is requesting out-of-band progress
	// notifications for this request (as represented by
	// notifications/progress). The value of this parameter is an
	// opaque token that will be attached to any subsequent
	// notifications. The receiver is not obligated to provide these
	// notifications.
	ProgressToken ProgressToken

	// AdditionalFields are any fields present in the Meta that are not
	// otherwise defined in the protocol.
	AdditionalFields map[string]any
}

// MarshalJSON writes m as [Meta.MarshalJSONTo] does, with encoding/json's own
// options.
func (m *Meta) MarshalJSON() ([]byte, error) {
	return jsonv2.Marshal(m, metaJSONOptions)
}

// metaJSONOptions are encoding/json's own, so [Meta.MarshalJSON] writes what
// json.Marshal would and [Meta.UnmarshalJSON]'s single pass reads as
// json.Unmarshal does: duplicate names allowed, the last one winning.
var metaJSONOptions = json.DefaultOptionsV1()

// MarshalJSONTo writes the fields as one object into the encoder of the value
// being marshaled, keys in the order json.Marshal gives a map's, with
// progressToken among them unless a field of that name overrides it. Written
// straight out, a _meta needs no merged copy of its map and no buffer of its
// own; json.Marshal calls it in preference to MarshalJSON. A value that cannot
// be encoded fails the write with the encoder left within the object, as
// encoding/json/v2's own types leave it; json.Marshal and json.Encoder write
// nothing of a value that failed.
func (m *Meta) MarshalJSONTo(enc *jsontext.Encoder) error {
	_, overridden := m.AdditionalFields["progressToken"]
	withToken := m.ProgressToken != nil && !overridden
	count := len(m.AdditionalFields)
	if withToken {
		count++
	}
	keys := make([]string, 0, count)
	for key := range m.AdditionalFields {
		keys = append(keys, key)
	}
	if withToken {
		keys = append(keys, "progressToken")
	}
	slices.Sort(keys)

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, key := range keys {
		value := m.AdditionalFields[key]
		if withToken && key == "progressToken" {
			value = m.ProgressToken
		}
		if err := writeMetaName(enc, key); err != nil {
			return err
		}
		// A value is written by encoding/json's own rules, whatever the
		// enclosing encoder's options, as MarshalJSON's output always was.
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := enc.WriteValue(jsontext.Value(encoded)); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// writeMetaName writes a key as json.Marshal writes a map's: a key no encoder
// escapes goes straight in; any other is encoded by encoding/json first, so
// the enclosing encoder's options do not change it.
func writeMetaName(enc *jsontext.Encoder, key string) error {
	if plainMetaName(key) {
		return enc.WriteToken(jsontext.String(key))
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		return err
	}
	return enc.WriteValue(jsontext.Value(encoded))
}

// plainMetaName reports whether every encoder writes key as it is: printable
// ASCII with no quote, backslash or HTML-sensitive character.
func plainMetaName(key string) bool {
	for i := 0; i < len(key); i++ {
		switch c := key[i]; {
		case c < 0x20, c >= 0x7f, c == '"', c == '\\', c == '<', c == '>', c == '&':
			return false
		}
	}
	return true
}

// UnmarshalJSON reads _meta into the map json.Unmarshal into map[string]any
// builds, with the progress token taken out of it and an integer token kept
// exact: an int64, or a json.Number past it. A single pass reads what it can
// settle exactly as encoding/json does; any other input, an error or one
// past 64 KiB included, is read by json.Unmarshal itself, so the result and
// the error are the same either way.
func (m *Meta) UnmarshalJSON(data []byte) error {
	if m.unmarshalFast(data) {
		return nil
	}
	return m.unmarshalJSONSlow(data)
}

// unmarshalJSONSlow reads _meta with json.Unmarshal into a map, then reads a
// numeric progress token again on its own to keep it exact.
func (m *Meta) unmarshalJSONSlow(data []byte) error {
	raw := make(map[string]any)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	token := raw["progressToken"]
	if _, number := token.(float64); number {
		var exact struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		}
		if err := json.Unmarshal(data, &exact); err != nil {
			return err
		}
		var err error
		if token, err = decodeProgressToken(exact.ProgressToken); err != nil {
			return err
		}
	}
	m.ProgressToken = token
	delete(raw, "progressToken")
	m.AdditionalFields = raw
	return nil
}

// decodeProgressToken decodes a progress token, a string or a number, with an
// integer kept exact as a request id is: an int64, or a json.Number past it,
// so the token comes back on its notifications as the request sent it. Any
// other number is a float64.
func decodeProgressToken(data []byte) (ProgressToken, error) {
	if isIntegerLiteral(data) {
		i, err := strconv.ParseInt(string(data), 10, 64)
		if err == nil {
			return i, nil
		}
		return json.Number(data), nil
	}
	var token any
	err := json.Unmarshal(data, &token)
	return token, err
}

// NewMetaFromMap builds metadata from m without changing or keeping the
// caller's map: the top level is copied, nested values are shared
// (upstream #1057).
func NewMetaFromMap(m map[string]any) *Meta {
	return metaFromOwnedMap(maps.Clone(m))
}

// metaFromOwnedMap builds metadata from a map the caller gives up, as the
// parsers do with the map they just decoded: no copy.
func metaFromOwnedMap(m map[string]any) *Meta {
	progressToken := m["progressToken"]
	delete(m, "progressToken")

	return &Meta{
		ProgressToken:    progressToken,
		AdditionalFields: m,
	}
}

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// defaultDiscoverProbeTimeout bounds the server/discover probe sent during
// Initialize. Servers predating protocol version 2026-07-28 answer an unknown
// method with MethodNotFound, but some ignore it entirely, which on stdio
// would otherwise block the handshake indefinitely.
const defaultDiscoverProbeTimeout = 5 * time.Second

// Protocol version 2026-07-28 removed the initialize handshake: a client
// declares its protocol version, identity, and capabilities in the _meta of
// every request, and may learn a server's capabilities up front through the
// server/discover RPC (SEP-2575).
//
// This client speaks both eras. Connect probes with server/discover; if that
// fails with anything other than a recognized modern error, it falls back to
// initialize and stays on the legacy revision for the rest of the connection.

// era is the state a connection settled on: the protocol version in effect
// ("" before Initialize), this client's identity and capabilities, and the
// server's capabilities. A Client holds one behind an atomic pointer and
// every request reads one snapshot of it; Initialize works on a copy and
// publishes it once, so a request beside a re-Initialize sees the era
// before or after, never the probe's.
type era struct {
	version            string
	clientInfo         mcp.Implementation
	clientCapabilities mcp.ClientCapabilities
	serverCapabilities mcp.ServerCapabilities
	// meta is the era's _meta entries, encoded when it was published.
	meta *eraMeta
}

// eraMeta is the _meta entries a request of a protocol version 2026-07-28
// era carries, encoded once: object is the whole _meta of a request without
// one of its own, and the entries are merged into one that has. of is the
// era they were encoded for; a copy of it, such as Initialize's probe, has
// its own address and encodes its own.
type eraMeta struct {
	of           *era
	version      json.RawMessage
	capabilities json.RawMessage
	info         json.RawMessage
	object       json.RawMessage
}

// currentEra is the era in effect; a Client built as a literal has an
// empty one.
func (c *Client) currentEra() *era {
	if e := c.era.Load(); e != nil {
		return e
	}
	return &era{}
}

// updateEra publishes a copy of the era changed by update.
func (c *Client) updateEra(update func(*era)) {
	next := *c.currentEra()
	update(&next)
	c.publishEra(&next)
}

// publishEra makes e the era in effect, with its _meta entries encoded. A
// published era is never changed again.
func (c *Client) publishEra(e *era) {
	e.meta = c.encodeEraMeta(e)
	c.era.Store(e)
}

// encodeEraMeta encodes the _meta entries of era e; an era before protocol
// version 2026-07-28 has none. An entry that does not encode is left out.
func (c *Client) encodeEraMeta(e *era) *eraMeta {
	if !mcp.IsModernProtocol(e.version) {
		return nil
	}
	m := &eraMeta{of: e}
	entries := make(map[string]json.RawMessage, 3)
	put := func(key string, value any) json.RawMessage {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		entries[key] = encoded
		return encoded
	}
	m.version = put(mcp.MetaKeyProtocolVersion, e.version)
	m.capabilities = put(mcp.MetaKeyClientCapabilities, c.effectiveCapabilities(e))
	if e.clientInfo.Name != "" {
		m.info = put(mcp.MetaKeyClientInfo, e.clientInfo)
	}
	m.object, _ = json.Marshal(entries)
	return m
}

// requestMeta is the _meta entries of era e: those encoded when it was
// published, or, for an era that never was, encoded now.
func (c *Client) requestMeta(e *era) *eraMeta {
	if m := e.meta; m != nil && m.of == e {
		return m
	}
	return c.encodeEraMeta(e)
}

// isModern reports whether the client is speaking the stateless protocol core.
func (c *Client) isModern() bool {
	return mcp.IsModernProtocol(c.currentEra().version)
}

// modulePath is this SDK's module, as its build information names it.
const modulePath = "github.com/mark3labs/mcp-go"

var (
	defaultInfoOnce sync.Once
	defaultInfo     mcp.Implementation
)

// defaultClientInfo is the clientInfo a client sends when its caller named
// none: this SDK, at the version the binary was built against, or
// "(devel)" when built from its own tree.
func defaultClientInfo() mcp.Implementation {
	defaultInfoOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			info = nil
		}
		defaultInfo = clientInfoFrom(info)
	})
	return defaultInfo
}

// clientInfoFrom names this SDK at the version the build information records
// for it: a replaced dependency carries the replacement's version, and a
// replacement without one (a local checkout) is "(devel)".
func clientInfoFrom(info *debug.BuildInfo) mcp.Implementation {
	result := mcp.Implementation{Name: "mcp-go", Version: "(devel)"}
	if info == nil {
		return result
	}
	for _, dep := range info.Deps {
		if dep.Path != modulePath {
			continue
		}
		if dep.Replace != nil {
			dep = dep.Replace
		}
		if dep.Version != "" {
			result.Version = dep.Version
		}
		return result
	}
	if info.Main.Path == modulePath && info.Main.Version != "" {
		result.Version = info.Main.Version
	}
	return result
}

// legacyRequiringTransport is implemented by transports that have been
// configured in a way only protocol versions before 2026-07-28 can satisfy,
// such as a Streamable HTTP transport using continuous listening.
type legacyRequiringTransport interface {
	RequiresLegacyProtocol() bool
}

// transportRequiresLegacyProtocol reports whether the transport rules out the
// stateless protocol core.
func (c *Client) transportRequiresLegacyProtocol() bool {
	legacy, ok := c.transport.(legacyRequiringTransport)
	return ok && legacy.RequiresLegacyProtocol()
}

// ProtocolVersion returns the protocol version in effect for this client, or
// "" before the connection has been established.
func (c *Client) ProtocolVersion() string {
	return c.currentEra().version
}

// effectiveCapabilities returns the client capabilities to declare in era e,
// merging the configured set with the capabilities implied by the registered
// handlers.
func (c *Client) effectiveCapabilities(e *era) mcp.ClientCapabilities {
	modern := mcp.IsModernProtocol(e.version)
	capabilities := e.clientCapabilities
	if c.samplingHandler != nil && capabilities.Sampling == nil {
		capabilities.Sampling = &mcp.SamplingCapability{}
	}
	// The listChanged sub-capability left with its notification in
	// protocol version 2026-07-28; it is declared on the handshake era only,
	// whether synthesized for a handler or supplied by the caller.
	switch {
	case c.rootsHandler != nil && capabilities.Roots == nil:
		capabilities.Roots = &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{ListChanged: !modern}
	case capabilities.Roots != nil && capabilities.Roots.ListChanged && modern:
		capabilities.Roots = &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{}
	}
	if c.elicitationHandler != nil && capabilities.Elicitation == nil {
		capabilities.Elicitation = &mcp.ElicitationCapability{}
	}
	return capabilities
}

// applyRequestMeta merges the per-request protocol metadata that protocol
// version 2026-07-28 requires into a request's params, as a request of era
// e.
//
// It is a no-op for legacy connections, whose protocol version, identity, and
// capabilities were fixed by the initialize handshake. The caller's own
// _meta entries are carried as the bytes they encode to, so an integer
// progress token keeps every digit. The params are returned encoded, so the
// headers and the transport do not encode them again.
func (c *Client) applyRequestMeta(e *era, params any) any {
	if !mcp.IsModernProtocol(e.version) {
		return params
	}

	encoded := []byte("{}")
	if params != nil {
		var err error
		if encoded, err = json.Marshal(params); err != nil {
			return params
		}
	}
	m := c.requestMeta(e)
	level := c.currentLogLevel()

	// Params with no _meta of their own, and no log level to declare, take
	// the era's _meta whole, as their last member. Any mention of "_meta",
	// even inside a value, takes the merge below.
	if n := len(encoded); level == "" && m.object != nil && n >= 2 && encoded[0] == '{' && encoded[n-1] == '}' &&
		!bytes.Contains(encoded, []byte(`"_meta"`)) {
		spliced := make([]byte, 0, n+len(`,"_meta":`)+len(m.object))
		spliced = append(spliced, encoded[:n-1]...)
		if n > 2 {
			spliced = append(spliced, ',')
		}
		spliced = append(spliced, `"_meta":`...)
		spliced = append(spliced, m.object...)
		return json.RawMessage(append(spliced, '}'))
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		// Params that are not a JSON object cannot carry _meta.
		return params
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := fields["_meta"]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	if m.version != nil {
		meta[mcp.MetaKeyProtocolVersion] = m.version
	}
	if m.capabilities != nil {
		meta[mcp.MetaKeyClientCapabilities] = m.capabilities
	}
	if m.info != nil {
		meta[mcp.MetaKeyClientInfo] = m.info
	}
	if level != "" {
		if encodedLevel, err := json.Marshal(level); err == nil {
			meta[mcp.MetaKeyLogLevel] = encodedLevel
		}
	}

	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return params
	}
	fields["_meta"] = encodedMeta
	merged, err := json.Marshal(fields)
	if err != nil {
		return fields
	}
	return json.RawMessage(merged)
}

// currentLogLevel returns the per-request log level to declare, if any.
func (c *Client) currentLogLevel() mcp.LoggingLevel {
	c.logLevelMu.RLock()
	defer c.logLevelMu.RUnlock()
	return c.logLevel
}

// applyStandardHeaders adds the Mcp-Protocol-Version header of era e, and
// the Mcp-Method and Mcp-Name headers that protocol version 2026-07-28
// requires on Streamable HTTP requests (SEP-2243), as a request of era e.
//
// Transports other than Streamable HTTP ignore them, so on a modern
// connection they are set unconditionally. The version goes on a request
// of any era that has one when the transport is an HTTP connection, so the
// transport's own copy never stands in for it: a probe sets that copy to
// the version it tries while requests of the era in effect are in flight,
// and a probe goes out before any version was negotiated.
func (c *Client) applyStandardHeaders(e *era, header http.Header, method string, params any) http.Header {
	if e.version == "" {
		return header
	}
	if !mcp.IsModernProtocol(e.version) {
		if _, ok := c.transport.(transport.HTTPConnection); !ok {
			return header
		}
		header = header.Clone()
		if header == nil {
			header = make(http.Header, 1)
		}
		header.Set(mcp.HeaderProtocolVersion, e.version)
		return header
	}

	// Params applyRequestMeta encoded are not encoded again.
	encoded, ok := params.(json.RawMessage)
	if !ok {
		var err error
		if encoded, err = json.Marshal(params); err != nil {
			return header
		}
	}

	standard := mcp.StandardHeaders(e.version, mcp.MCPMethod(method), encoded)
	if header == nil {
		header = make(http.Header, len(standard)+1)
	} else {
		header = header.Clone()
	}
	header.Set(mcp.HeaderProtocolVersion, e.version)
	for name, value := range standard {
		header.Set(name, value)
	}

	// Tool parameters annotated with x-mcp-header must be mirrored into
	// Mcp-Param-* headers so gateways can route on them.
	if mcp.MCPMethod(method) == mcp.MethodToolsCall {
		if tool := c.toolForCall(encoded); tool != nil {
			for name, value := range mcp.GenerateParamHeaders(tool, encoded) {
				header.Set(name, value)
			}
		}
	}

	return header
}

// toolForCall returns the cached tool definition named by a tools/call
// request, or nil when the client has not listed tools yet.
func (c *Client) toolForCall(params json.RawMessage) *mcp.Tool {
	var call struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil
	}
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	tool, ok := c.knownTools[call.Name]
	if !ok {
		return nil
	}
	return &tool
}

// InvalidToolHandler is called for each tool definition that ListTools,
// ListToolsByPage and IterTools exclude because its x-mcp-header annotations
// violate SEP-2243. err is the error returned by
// mcp.ValidateParamHeaderAnnotations.
//
// The handler runs synchronously on the goroutine that listed the tools, once
// per excluded tool and page, with that request's context, so a caller can
// correlate the call with its own span or request. It must not block.
type InvalidToolHandler func(ctx context.Context, tool mcp.Tool, err error)

// WithInvalidToolHandler sets the handler notified about tools excluded for
// their invalid x-mcp-header annotations.
//
// Exclusion itself is unchanged: the tool is still dropped from the listing and
// forgotten from the cache, and one warning per listing is still logged.
// Without this option, that warning, which names only the first excluded
// tool, is the only signal that a tool disappeared.
func WithInvalidToolHandler(handler InvalidToolHandler) ClientOption {
	return func(c *Client) {
		c.invalidToolHandler = handler
	}
}

// withoutInvalidHeaderTools drops the tool definitions whose x-mcp-header
// annotations break the constraints of SEP-2243. A client using Streamable
// HTTP MUST exclude such a tool from the tools/list result, so that one bad
// definition does not stop the others from being used. A definition cached
// from an earlier listing is forgotten too, so the tool is not called with
// headers mirrored from it. Other transports may ignore the annotations, and
// connections before protocol version 2026-07-28 do not mirror them at all.
func (c *Client) withoutInvalidHeaderTools(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	if !c.mirrorsHeaders() {
		return tools
	}
	valid := make([]mcp.Tool, 0, len(tools))
	var excluded int
	var first string
	var firstErr error
	for _, tool := range tools {
		if err := mcp.ValidateParamHeaderAnnotations(&tool); err != nil {
			if excluded == 0 {
				first, firstErr = tool.Name, err
			}
			excluded++
			if c.invalidToolHandler != nil {
				c.invalidToolHandler(ctx, tool, err)
			}
			c.forgetTool(tool.Name)
			continue
		}
		valid = append(valid, tool)
	}
	if excluded > 0 {
		// One record per listing, whatever a server lists: the first name and
		// its reason stand for the rest.
		slog.WarnContext(ctx, "mcp: excluding tools with invalid x-mcp-header annotations", "excluded", excluded, "first", first, "err", firstErr)
	}
	return valid
}

// forgetTool drops a cached tool definition.
func (c *Client) forgetTool(name string) {
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	delete(c.knownTools, name)
}

// mirrorsHeaders reports whether this connection mirrors tool parameters
// into Mcp-Param-* headers: a modern connection over Streamable HTTP. Only
// such a connection keeps tool definitions for it.
func (c *Client) mirrorsHeaders() bool {
	if !c.isModern() {
		return false
	}
	_, ok := c.transport.(transport.HTTPConnection)
	return ok
}

// rememberTools caches the tool definitions of one page so that subsequent
// tools/call requests can mirror x-mcp-header annotated parameters into HTTP
// headers; a connection that mirrors none keeps none.
func (c *Client) rememberTools(tools []mcp.Tool) {
	if len(tools) == 0 || !c.mirrorsHeaders() {
		return
	}
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	if c.knownTools == nil {
		c.knownTools = make(map[string]mcp.Tool, len(tools))
	}
	for _, tool := range tools {
		c.knownTools[tool.Name] = tool
	}
}

// replaceTools makes the cache the server's whole list, as a complete walk
// of tools/list gave it: definitions the server no longer lists go.
func (c *Client) replaceTools(tools []mcp.Tool) {
	if !c.mirrorsHeaders() {
		return
	}
	known := make(map[string]mcp.Tool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = tool
	}
	c.toolsMu.Lock()
	defer c.toolsMu.Unlock()
	c.knownTools = known
}

// Discover asks the server which protocol versions it supports, what its
// capabilities are, and who it is, without performing the legacy initialize
// handshake (SEP-2575).
//
// Servers implementing protocol version 2026-07-28 or later must support this
// method. Against an older server it fails, which is how [Client.Initialize]
// detects that it must fall back to the handshake.
func (c *Client) Discover(ctx context.Context, request mcp.DiscoverRequest) (*mcp.DiscoverResult, error) {
	// Before a version is in effect the probe asks at the newest this client
	// speaks; the era in effect is left as it is.
	e := c.currentEra()
	if !mcp.IsModernProtocol(e.version) {
		probe := *e
		probe.version = mcp.LATEST_PROTOCOL_VERSION
		e = &probe
	}
	return c.discoverIn(ctx, e, request.Params, outboundHeader(request.Header, request.Method))
}

// discoverIn sends server/discover as a request of era e.
func (c *Client) discoverIn(ctx context.Context, e *era, params mcp.DiscoverParams, header http.Header) (*mcp.DiscoverResult, error) {
	response, err := c.sendRequestIn(ctx, e, string(mcp.MethodServerDiscover), params, header)
	if err != nil {
		return nil, err
	}

	// The result type is judged before the shape: a type the request may
	// not be answered with is a modern server's answer, where a result
	// without the discover fields may be a legacy server's.
	var result mcp.DiscoverResult
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal discover result: %w", err)
	}
	if err := c.checkResultType(result.ResultType); err != nil {
		return nil, err
	}
	if err := validateDiscoverResult(*response); err != nil {
		return nil, fmt.Errorf("invalid server/discover result: %w", err)
	}
	return &result, nil
}

// validateDiscoverResult checks the fields that tell a discovery result apart
// from a legacy server's unrelated successful result, since some legacy
// servers answer an unknown method with an empty result. It is not a full
// schema check: an absent resultType or cache hint decodes as before.
func validateDiscoverResult(data json.RawMessage) error {
	var shape struct {
		SupportedVersions []*string                  `json:"supportedVersions"`
		Capabilities      map[string]json.RawMessage `json:"capabilities"`
		Meta              json.RawMessage            `json:"_meta"`
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	if shape.SupportedVersions == nil {
		return errors.New("supportedVersions must be a non-null array of strings")
	}
	for _, version := range shape.SupportedVersions {
		if version == nil {
			return errors.New("supportedVersions must contain only strings")
		}
	}
	if shape.Capabilities == nil {
		return errors.New("capabilities must be an object")
	}
	if len(shape.Meta) == 0 {
		return nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(shape.Meta, &meta); err != nil {
		return err
	}
	if meta == nil {
		return errors.New("_meta must be an object")
	}
	if raw, ok := meta[mcp.MetaKeyServerInfo]; ok {
		var info struct {
			Name    *string `json:"name"`
			Version *string `json:"version"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			return err
		}
		if info.Name == nil || info.Version == nil {
			return errors.New("_meta serverInfo must be an object with string name and version")
		}
	}
	return nil
}

// negotiateModern attempts to establish a modern, stateless connection.
//
// It sends server/discover at the client's preferred version. If the server
// rejects that version but names the ones it does support, the request is
// retried once at the highest mutually supported modern version, even when it
// matches the rejected version. A discovery result is accepted at the version
// the probe was sent with when the server lists it, and otherwise at the
// highest modern version both sides support.
//
// It also reports whether the server answered as a modern one that the legacy
// initialize handshake cannot reach: it shares a modern version with the
// client, or it lists only versions the client does not speak. A failure then
// is the caller's to see. Otherwise the server may be legacy, or pinned to a
// legacy revision it lists, and the caller falls back to the handshake.
//
// The probe is bounded by a timeout: a well-behaved JSON-RPC server answers an
// unknown method with MethodNotFound, but a server predating this method may
// simply ignore it, which on stdio would otherwise block forever.
func (c *Client) negotiateModern(ctx context.Context, preferred string, next *era, header http.Header) (*mcp.DiscoverResult, bool, error) {
	if preferred == "" {
		preferred = mcp.LATEST_PROTOCOL_VERSION
	}

	probeCtx := ctx
	if timeout := c.discoverProbeTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// The version travels in each request's _meta and, over HTTP, in the
	// Mcp-Protocol-Version header, which must agree: the probe's era carries
	// it, and the transport's copy is set for the probe and restored on
	// failure. The era in effect is left alone until the caller publishes
	// next.
	previous := next.version
	probe := *next
	probe.version = preferred
	c.setTransportVersion(preferred)

	var err error
	modern := false
	for attempt := range 2 {
		var result *mcp.DiscoverResult
		result, err = c.discoverIn(probeCtx, &probe, mcp.DiscoverParams{}, header)
		if err == nil {
			version := discoveredVersion(probe.version, result.SupportedVersions)
			if version == "" {
				// A server that answers server/discover is modern; it is
				// reached through the handshake only when it lists a legacy
				// version this client speaks, and never once a rejection has
				// named a modern version both sides support.
				c.setTransportVersion(previous)
				return nil, modern || !listsSharedVersion(result.SupportedVersions), fmt.Errorf("server/discover result lists no modern version this client supports: %s", summarizeVersions(result.SupportedVersions))
			}
			next.version = version
			return result, true, nil
		}

		// A modern error other than a version rejection, or a discover
		// result of a type the request may not be answered with, is a modern
		// server's answer (Transports.md rows 10 and 41): it is the caller's
		// to see.
		var mismatch mcp.HeaderMismatchError
		if errors.As(err, &mismatch) || mcp.IsMissingRequiredClientCapability(err) || errors.Is(err, errUnacceptedResultType) {
			modern = true
			break
		}

		// A rejection naming a modern version both sides support identifies a
		// modern server: renegotiate rather than fall back. The retry stays
		// bounded even when that version is the one just rejected.
		var unsupported mcp.UnsupportedProtocolVersionError
		if !errors.As(err, &unsupported) || unsupported.Supported == nil || attempt > 0 {
			break
		}
		negotiated := mcp.NegotiateMutuallySupportedVersion(unsupported.Supported)
		if !mcp.IsModernProtocol(negotiated) {
			// The rejection is a modern server's. With no modern version in
			// common, the client falls back to the handshake only when the
			// server lists a legacy version it speaks.
			modern = negotiated == ""
			break
		}
		modern = true
		probe.version = negotiated
		c.setTransportVersion(negotiated)
	}

	c.setTransportVersion(previous)
	return nil, modern, err
}

// summarizeVersions names the first few of a server's versions and the
// count of the rest, so a server's list is not copied into an error whole.
func summarizeVersions(versions []string) string {
	const shown = 5
	if len(versions) <= shown {
		return fmt.Sprintf("%q", versions)
	}
	return fmt.Sprintf("%q and %d more", versions[:shown], len(versions)-shown)
}

// listsSharedVersion reports whether supported names any version this client
// speaks.
func listsSharedVersion(supported []string) bool {
	return mcp.NegotiateMutuallySupportedVersion(supported) != ""
}

// discoveredVersion picks the version to continue with after server/discover
// lists supported: the one the probe was sent with, when listed, or else the
// newest modern version both sides support. It returns "" when there is none.
func discoveredVersion(sent string, supported []string) string {
	if slices.Contains(supported, sent) {
		return sent
	}
	if negotiated := mcp.NegotiateMutuallySupportedVersion(supported); mcp.IsModernProtocol(negotiated) {
		return negotiated
	}
	return ""
}

// discoverProbeTimeout returns how long to wait for a server/discover reply
// before concluding the server is legacy.
func (c *Client) discoverProbeTimeout() time.Duration {
	if c.discoverTimeout != 0 {
		return c.discoverTimeout
	}
	return defaultDiscoverProbeTimeout
}

// initializeResultFromDiscover renders a DiscoverResult in the shape of an
// InitializeResult, so that callers of [Client.Initialize] observe the same
// value regardless of which era the connection settled on.
func initializeResultFromDiscover(protocolVersion string, result *mcp.DiscoverResult) *mcp.InitializeResult {
	initialized := &mcp.InitializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    result.Capabilities,
		Instructions:    result.Instructions,
	}
	if info := result.GetResultMeta().ServerInfo(); info != nil {
		initialized.ServerInfo = *info
	}
	return initialized
}

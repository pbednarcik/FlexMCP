package server

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"

	"github.com/mark3labs/mcp-go/mcp"
)

// defaultCacheTTLMs is the cache freshness hint applied to list and read
// results when the server does not configure one. Zero means "always
// revalidate", which preserves the polling behaviour of earlier protocol
// versions while still satisfying the SEP-2549 requirement that the field be
// present.
const defaultCacheTTLMs int64 = 0

// defaultCacheScope is the scope advertised when a server configures no
// caching hints. It is fail-closed: results may be reused only within the
// authorization context that fetched them, so a shared intermediary cannot
// serve one principal's data to another. Servers that want sharing declare it
// with [WithCacheHints].
const defaultCacheScope = mcp.CacheScopePrivate

// cacheHints describes the SEP-2549 caching hints a server advertises on a
// list or read result.
type cacheHints struct {
	ttlMs int64
	scope mcp.CacheScope
}

// decorateResponse applies the result metadata required by protocol version
// 2026-07-28 to an outgoing response.
//
// For modern requests it stamps resultType, the server identity in _meta, and
// the SEP-2549 caching hints. For legacy requests it is a no-op, so responses
// remain byte-identical to earlier releases.
func (s *MCPServer) decorateResponse(
	_ context.Context,
	info *RequestProtocolInfo,
	method mcp.MCPMethod,
	resp mcp.JSONRPCMessage,
) mcp.JSONRPCMessage {
	if info == nil || !info.Modern || resp == nil {
		return resp
	}

	response, ok := resp.(mcp.JSONRPCResponse)
	if !ok || response.Result == nil {
		return resp
	}

	decorated, ok := s.decorateResult(response.Result, method)
	if !ok {
		return resp
	}
	response.Result = decorated
	return response
}

// decorateResult stamps the modern result metadata onto a copy of a result.
//
// The handler's own result is never written: a handler may return the same
// one to every call, so a stamp written into it would race with the next
// call's and would reach a legacy client. The copy is addressable, which is
// what makes the pointer-receiver decoration interfaces reachable for the
// results the generated dispatcher stores by value.
func (s *MCPServer) decorateResult(result any, method mcp.MCPMethod) (any, bool) {
	value := reflect.ValueOf(result)
	if !value.IsValid() {
		return nil, false
	}

	byValue := value.Kind() != reflect.Pointer
	if !byValue {
		if value.IsNil() {
			return nil, false
		}
		value = value.Elem()
	}
	pointer := reflect.New(value.Type())
	pointer.Elem().Set(value)

	metadata, ok := reflect.TypeAssert[mcp.ResultMetadata](pointer)
	if !ok {
		return nil, false
	}

	// The copy still shares the handler's _meta: it gets its own before a
	// field is written.
	if meta := metadata.GetResultMeta(); meta != nil {
		own := *meta
		own.AdditionalFields = maps.Clone(meta.AdditionalFields)
		metadata.SetResultMeta(&own)
	}

	// resultType is required from 2026-07-28 onward. A handler that already
	// set it - to signal input_required, for example - keeps its value.
	if metadata.GetResultType() == "" {
		metadata.SetResultType(mcp.ResultTypeComplete)
	}

	// Servers SHOULD identify themselves in every result.
	if meta := metadata.EnsureResultMeta(); meta != nil && meta.ServerInfo() == nil {
		meta.SetMetaField(mcp.MetaKeyServerInfo, s.boxedServerInfo())
	}

	// ttlMs and cacheScope are required on list and read results.
	if methodReturnsCacheableResult(method) {
		if cacheable, ok := reflect.TypeAssert[mcp.CacheHintSetter](pointer); ok {
			applyDefaultCacheHints(cacheable, s.cacheHintsFor(method))
		}
	}

	if byValue {
		return pointer.Elem().Interface(), true
	}
	return pointer.Interface(), true
}

// boxedServerInfo is the server's identity as result _meta carries it: boxed
// once by NewMCPServer, or now for a server it did not build.
func (s *MCPServer) boxedServerInfo() any {
	if s.serverInfo != nil {
		return s.serverInfo
	}
	return s.serverImplementation()
}

// cacheHintsFor returns the caching hints configured for the given method,
// falling back to the server-wide default.
func (s *MCPServer) cacheHintsFor(method mcp.MCPMethod) cacheHints {
	s.capabilitiesMu.RLock()
	defer s.capabilitiesMu.RUnlock()

	if s.cacheHints != nil {
		if configured, ok := s.cacheHints[method]; ok {
			return configured
		}
		if configured, ok := s.cacheHints[""]; ok {
			return configured
		}
	}
	return cacheHints{ttlMs: defaultCacheTTLMs, scope: defaultCacheScope}
}

// applyDefaultCacheHints populates caching hints on a result that has not
// already set them.
func applyDefaultCacheHints(result mcp.CacheHintSetter, hints cacheHints) {
	if cacheable, ok := result.(interface{ TTL() (int64, bool) }); ok {
		if _, alreadySet := cacheable.TTL(); alreadySet {
			return
		}
	}
	result.SetCacheHints(hints.ttlMs, hints.scope)
}

// methodReturnsCacheableResult reports whether protocol version 2026-07-28
// requires ttlMs and cacheScope on the result of the given method.
func methodReturnsCacheableResult(method mcp.MCPMethod) bool {
	switch method {
	case mcp.MethodToolsList,
		mcp.MethodPromptsList,
		mcp.MethodResourcesList,
		mcp.MethodResourcesTemplatesList,
		mcp.MethodResourcesRead,
		mcp.MethodServerDiscover:
		return true
	default:
		return false
	}
}

// errorResponseForProtocolError converts a protocol-level validation failure
// into the JSON-RPC error the specification prescribes.
func errorResponseForProtocolError(id any, err error) mcp.JSONRPCMessage {
	if unsupported, ok := errors.AsType[mcp.UnsupportedProtocolVersionError](err); ok {
		response := unsupported.JSONRPCError()
		response.ID = mcp.NewRequestId(id)
		return response
	}

	if mismatch, ok := errors.AsType[mcp.HeaderMismatchError](err); ok {
		response := mismatch.JSONRPCError()
		response.ID = mcp.NewRequestId(id)
		return response
	}

	if missing, ok := errors.AsType[mcp.MissingRequiredClientCapabilityError](err); ok {
		response := missing.JSONRPCError()
		response.ID = mcp.NewRequestId(id)
		return response
	}

	return createErrorResponse(id, mcp.INVALID_PARAMS, err.Error())
}

// validateStandardHeadersForMessage checks the Mcp-Method, Mcp-Name, and
// Mcp-Param-* headers against the JSON-RPC message body, as required from
// protocol version 2026-07-28 (SEP-2243).
//
// Requests that did not arrive over HTTP carry no headers, so validation is
// skipped: the header contract binds the Streamable HTTP transport only.
func (s *MCPServer) validateStandardHeadersForMessage(
	ctx context.Context,
	headers http.Header,
	protocolVersion string,
	method mcp.MCPMethod,
	message json.RawMessage,
) error {
	if len(headers) == 0 || headers.Get(mcp.HeaderProtocolVersion) == "" {
		return nil
	}

	var wrapper struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(message, &wrapper); err != nil {
		return nil
	}

	if err := mcp.ValidateStandardHeaders(headers.Get, protocolVersion, method, wrapper.Params); err != nil {
		return err
	}

	// Tool parameters annotated with x-mcp-header travel in headers as well as
	// in the body, and the two must agree.
	if method != mcp.MethodToolsCall {
		return nil
	}
	bindings, ok := s.headerBindingsForCall(wrapper.Params)
	if !ok {
		return nil
	}
	return checkParamHeaders(headers, bindings, message)
}

// lookupHeader reports the first value of a header and whether it was sent at
// all, so that a header sent with an empty value is not taken for a missing
// one.
func lookupHeader(headers http.Header) func(string) (string, bool) {
	return func(name string) (string, bool) {
		values := headers.Values(name)
		if len(values) == 0 {
			return "", false
		}
		return values[0], true
	}
}

// headerBindingsForCall resolves the x-mcp-header bindings of the tool named
// by a tools/call request, cached from the tool's registration. It returns
// ok=false when the tool is unknown, leaving the not-found error to the
// handler.
func (s *MCPServer) headerBindingsForCall(params json.RawMessage) ([]headerBinding, bool) {
	var call struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, false
	}
	return s.headerBindingsForTool(call.Name)
}

// headerBindingsForTool is [MCPServer.headerBindingsForCall] for a tool name
// already read from the request.
func (s *MCPServer) headerBindingsForTool(name string) ([]headerBinding, bool) {
	if name == "" {
		return nil, false
	}
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	if tool, ok := s.tools[name]; ok {
		return s.cachedHeaderBindings(name, &tool.Tool), true
	}
	if taskTool, ok := s.taskTools[name]; ok {
		return s.cachedHeaderBindings(name, &taskTool.Tool), true
	}
	return nil, false
}

// validateStandardHeadersOfRequest is
// [MCPServer.validateStandardHeadersForMessage] for a request already read
// into its envelope: the name the Mcp-Name header carries comes from it, and
// the message is read again only for a tool that binds parameters to headers,
// and then only for the bound values.
func (s *MCPServer) validateStandardHeadersOfRequest(
	headers http.Header,
	protocolVersion string,
	method mcp.MCPMethod,
	params *paramsPeek,
	message json.RawMessage,
) error {
	if len(headers) == 0 || headers.Get(mcp.HeaderProtocolVersion) == "" {
		return nil
	}
	bodyName := func() (string, bool) { return params.headerName(method) }
	if err := mcp.ValidateStandardHeadersWithName(headers.Get, protocolVersion, method, bodyName); err != nil {
		return err
	}

	if method != mcp.MethodToolsCall {
		return nil
	}
	name, ok := bodyName()
	if !ok {
		return nil
	}
	bindings, ok := s.headerBindingsForTool(name)
	if !ok || len(bindings) == 0 {
		return nil
	}
	return checkParamHeaders(headers, bindings, message)
}

// cachedHeaderBindings returns the bindings registration extracted for name.
// It reads the schema only when the map has no entry for the tool, so a
// registration path that forgets the cache degrades to the per-call read
// rather than switching the check off. Called with toolsMu held.
func (s *MCPServer) cachedHeaderBindings(name string, tool *mcp.Tool) []headerBinding {
	if bindings, ok := s.toolHeaderBindings[name]; ok {
		return bindings
	}
	return keepHeaderBindings(tool)
}

func createResponse(id any, result any) mcp.JSONRPCMessage {
	return mcp.NewJSONRPCResultResponse(mcp.NewRequestId(id), result)
}

func createErrorResponse(
	id any,
	code int,
	message string,
) mcp.JSONRPCMessage {
	return mcp.JSONRPCError{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(id),
		Error:   mcp.NewJSONRPCErrorDetails(code, message, nil),
	}
}

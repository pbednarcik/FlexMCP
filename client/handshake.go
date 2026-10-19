package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// Initialize establishes the connection with the server.
//
// It must be called after Start, and before any request methods.
//
// Protocol version 2026-07-28 removed the initialize handshake (SEP-2575), so
// this method first probes the server with server/discover. When the probe
// succeeds the connection is stateless and every subsequent request carries
// its own protocol metadata; the returned InitializeResult is rendered from
// the discovery response so that callers observe the same value in both eras.
//
// When the probe fails before a modern version shared with the server is
// found, the server is taken to be legacy, and the classic initialize
// handshake is performed instead. That covers a server that answers the probe
// but offers only legacy versions. Once a shared modern version is found, a
// negotiation failure is returned to the caller rather than falling back.
func (c *Client) Initialize(
	ctx context.Context,
	request mcp.InitializeRequest,
) (*mcp.InitializeResult, error) {
	// The negotiation works on a copy of the era, published once it is
	// settled: a request in flight beside it sees the era before or after,
	// never the probe's.
	next := *c.currentEra()
	if request.Params.ClientInfo.Name != "" {
		next.clientInfo = request.Params.ClientInfo
	}
	next.clientCapabilities = mergeClientCapabilities(next.clientCapabilities, request.Params.Capabilities)
	header := outboundHeader(request.Header, request.Method)

	preferred := request.Params.ProtocolVersion
	if preferred == "" {
		preferred = c.preferredVersion
	}

	// Try the stateless protocol core first, unless the caller pinned an
	// earlier revision or configured the transport in a way that rules it out.
	if !c.legacyOnly && !c.transportRequiresLegacyProtocol() &&
		(preferred == "" || mcp.IsModernProtocol(preferred)) {
		discovered, modern, err := c.negotiateModern(ctx, preferred, &next, header)
		if err == nil {
			next.serverCapabilities = discovered.Capabilities
			c.publishEra(&next)
			c.setTransportVersion(next.version)
			c.initialized.Store(true)
			return initializeResultFromDiscover(next.version, discovered), nil
		}
		if modern {
			return nil, err
		}
		// Fall through to the handshake: the server is not modern.
	}

	return c.initializeLegacy(ctx, preferred, &next, header)
}

// initializeLegacy performs the initialize/initialized handshake used by
// protocol versions up to and including 2025-11-25.
func (c *Client) initializeLegacy(
	ctx context.Context,
	preferred string,
	next *era,
	header http.Header,
) (*mcp.InitializeResult, error) {
	capabilities := c.effectiveCapabilities(next)

	// Ensure we send a params object with all required fields
	params := struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		ClientInfo      mcp.Implementation     `json:"clientInfo"`
		Capabilities    mcp.ClientCapabilities `json:"capabilities"`
	}{
		ProtocolVersion: preferred,
		ClientInfo:      next.clientInfo,
		Capabilities:    capabilities,
	}

	// The handshake cannot negotiate 2026-07-28 or later, so ask for the
	// newest revision that still uses it.
	if params.ProtocolVersion == "" || mcp.IsModernProtocol(params.ProtocolVersion) {
		params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	}

	// The handshake is a legacy request, carrying no per-request metadata.
	handshake := *next
	handshake.version = ""
	response, err := c.sendRequestIn(ctx, &handshake, "initialize", params, header)
	if err != nil {
		return nil, err
	}

	var result mcp.InitializeResult
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// Validate protocol version
	if !mcp.IsValidProtocolVersion(result.ProtocolVersion) {
		return nil, mcp.UnsupportedProtocolVersionError{Version: result.ProtocolVersion}
	}

	// Publish the era: the server's capabilities and the negotiated version.
	next.serverCapabilities = result.Capabilities
	next.version = result.ProtocolVersion
	c.publishEra(next)
	c.setTransportVersion(next.version)

	// Send initialized notification
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  string(mcp.MethodNotificationInitialized),
	}

	err = c.transport.SendNotification(ctx, notification)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to send initialized notification: %w",
			err,
		)
	}

	c.initialized.Store(true)
	return &result, nil
}

// setTransportVersion propagates the protocol version in effect to HTTP
// transports, which mirror it in the Mcp-Protocol-Version header.
func (c *Client) setTransportVersion(version string) {
	if httpConn, ok := c.transport.(transport.HTTPConnection); ok {
		httpConn.SetProtocolVersion(version)
	}
}

// mergeClientCapabilities overlays the capabilities declared on a request onto
// those configured at construction.
func mergeClientCapabilities(base, overlay mcp.ClientCapabilities) mcp.ClientCapabilities {
	if overlay.Extensions != nil {
		base.Extensions = overlay.Extensions
	}
	if overlay.Experimental != nil {
		base.Experimental = overlay.Experimental
	}
	if overlay.Roots != nil {
		base.Roots = overlay.Roots
	}
	if overlay.Sampling != nil {
		base.Sampling = overlay.Sampling
	}
	if overlay.Elicitation != nil {
		base.Elicitation = overlay.Elicitation
	}
	if overlay.Tasks != nil {
		base.Tasks = overlay.Tasks
	}
	return base
}

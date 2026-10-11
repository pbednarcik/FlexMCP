package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// isLoopbackHost reports whether addr refers to a loopback interface. addr
// may be a bare host ("localhost", "127.0.0.1", "::1", "[::1]") or a
// host:port pair ("localhost:3000", "127.0.0.1:3000", "[::1]:3000").
//
// It is used to implement DNS rebinding protection: a request that arrives
// on a loopback connection must also carry a loopback Host header, otherwise
// a malicious website could rebind its own domain to 127.0.0.1 and drive a
// victim's browser to interact with a local MCP server.
func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr might be a bare host without a port.
		host = strings.Trim(addr, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}

// isLoopbackOrigin reports whether origin, the value of an Origin header, is
// an http or https origin on a loopback host. An opaque origin ("null") or
// anything that is not a URL is not. The scheme and host are cut out by hand:
// url.Parse would allocate on every browser request for a three-field answer.
func isLoopbackOrigin(origin string) bool {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || (!strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https")) {
		return false
	}
	if host, _, found := strings.Cut(rest, "/"); found {
		rest = host
	}
	return rest != "" && isLoopbackHost(rest)
}

// isLoopbackAddr reports whether a connection's local address is a loopback
// address. A TCP address answers from its IP; rendering it as a string, as
// the generic path does, would allocate on every request.
func isLoopbackAddr(addr net.Addr) bool {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP.IsLoopback()
	}
	return isLoopbackHost(addr.String())
}

// rejectDNSRebinding applies DNS rebinding protection to r. When the
// connection's local address (http.LocalAddrContextKey) is a loopback
// address but the request's Host header is not a loopback value, or, with
// checkOrigin, its Origin header is present and not a loopback origin, it
// writes a 403 Forbidden response and returns true. Otherwise it returns
// false and writes nothing.
//
// Host catches the rebound request itself; Origin catches what the browser
// reports about the page that sent it, which rebinding cannot change. The
// server validates Origin on every connection and answers an invalid one
// with 403 (Transports.md row 14); a configured CORS allow list is the
// authority on Origin instead, and the caller passes checkOrigin false.
//
// Requests arriving via non-loopback addresses are never rejected: DNS
// rebinding attacks only target servers reachable at localhost from the
// victim's browser.
//
// See https://modelcontextprotocol.io/specification/2025-11-25/basic/security_best_practices#local-mcp-server-compromise
func rejectDNSRebinding(w http.ResponseWriter, r *http.Request, checkOrigin bool) bool {
	localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || localAddr == nil || !isLoopbackAddr(localAddr) {
		return false
	}
	if !isLoopbackHost(r.Host) {
		http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
		return true
	}
	if origin := r.Header.Get("Origin"); checkOrigin && origin != "" && !isLoopbackOrigin(origin) {
		http.Error(w, fmt.Sprintf("Forbidden: invalid Origin header %q", origin), http.StatusForbidden)
		return true
	}
	return false
}

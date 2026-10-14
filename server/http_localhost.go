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
// It is part of DNS rebinding protection: a request that arrives on a
// connection a rebinding attack can reach must name the server, otherwise a
// malicious website could rebind its own domain to the server's address and
// drive a victim's browser to interact with it.
func isLoopbackHost(addr string) bool {
	host := hostName(addr)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}

// hostName is host without its port and the brackets of an IPv6 literal.
func hostName(host string) string {
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		// host might be a bare host without a port.
		return strings.Trim(host, "[]")
	}
	return name
}

// originHost returns the host of origin, the value of an Origin header, when
// it is an http or https origin. An opaque origin ("null") or anything that
// is not a URL has none. The scheme and host are cut out by hand: url.Parse
// would allocate on every browser request for a three-field answer.
func originHost(origin string) (string, bool) {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || (!strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https")) {
		return "", false
	}
	if host, _, found := strings.Cut(rest, "/"); found {
		rest = host
	}
	return rest, rest != ""
}

// localAddrIP returns the IP of a connection's local address. A TCP address
// answers from its IP; rendering it as a string, as the generic path does,
// would allocate on every request. An IPv4 address a dual-stack listener
// reports in its IPv6 form is unmapped.
func localAddrIP(addr net.Addr) (netip.Addr, bool) {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip, ok := netip.AddrFromSlice(tcp.IP)
		return ip.Unmap(), ok
	}
	ip, err := netip.ParseAddr(hostName(addr.String()))
	return ip.Unmap(), err == nil
}

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598), which carrier-grade NAT
// and overlay networks such as Tailscale use: private in effect, though
// netip does not count it so.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// isRebindingTarget reports whether a server at ip is one a DNS rebinding
// attack reaches: a loopback, private, shared or link-local address, which
// the attacker's own network cannot reach but a victim's browser on it can.
// A public address is reachable without rebinding, so the attack gains
// nothing there.
func isRebindingTarget(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || sharedAddressSpace.Contains(ip)
}

// hostNamesServer reports whether host, a Host header or an origin's host,
// names the server at ip: a loopback name, ip itself, or one of allowed.
// Names are compared without their port, case-insensitively and without a
// trailing dot; an IP literal is compared as an address, so "[::1]" and
// "::1" are one.
func hostNamesServer(host string, ip netip.Addr, allowed []string) bool {
	if isLoopbackHost(host) {
		return true
	}
	name := hostName(host)
	hostIP, isIP := netip.ParseAddr(name)
	if isIP == nil {
		hostIP = hostIP.Unmap()
		if hostIP == ip {
			return true
		}
	}
	for _, entry := range allowed {
		entryName := hostName(entry)
		if isIP == nil {
			if entryIP, err := netip.ParseAddr(entryName); err == nil && entryIP.Unmap() == hostIP {
				return true
			}
			continue
		}
		if strings.EqualFold(strings.TrimSuffix(name, "."), strings.TrimSuffix(entryName, ".")) {
			return true
		}
	}
	return false
}

// rejectDNSRebinding applies DNS rebinding protection to r. When the
// connection's local address (http.LocalAddrContextKey) is one a rebinding
// attack reaches (isRebindingTarget), a request whose Origin header is
// present and does not name the server (hostNamesServer, with
// allowedHosts) is answered 403 Forbidden, and so, on a loopback address,
// is one whose Host header does not; then it returns true. Otherwise it
// returns false and writes nothing. checkOrigin false leaves Origin to a
// configured CORS allow list, the authority on it then.
//
// Only a browser can be rebound, and a browser sends Origin on every POST,
// so Origin catches the rebound request on any address: what the browser
// reports about the page that sent it, which rebinding cannot change. Host
// names the rebound domain too, but on a private address it names the
// server's own hostname as well, as a reverse proxy or a container network
// forwards it, so Host is checked only on a loopback address, where the
// local-server guidance asks for it. The server validates Origin on every
// connection and answers an invalid one with 403 (Transports.md row 14).
//
// Requests arriving on a public address are never rejected: the attacker
// reaches that server without rebinding.
//
// See https://modelcontextprotocol.io/specification/2025-11-25/basic/security_best_practices#local-mcp-server-compromise
func rejectDNSRebinding(w http.ResponseWriter, r *http.Request, checkOrigin bool, allowedHosts []string) bool {
	localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || localAddr == nil {
		return false
	}
	ip, ok := localAddrIP(localAddr)
	if !ok || !isRebindingTarget(ip) {
		return false
	}
	if ip.IsLoopback() && !hostNamesServer(r.Host, ip, allowedHosts) {
		http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q; a name the server is known by is allowed with WithAllowedHosts", r.Host), http.StatusForbidden)
		return true
	}
	if origin := r.Header.Get("Origin"); checkOrigin && origin != "" {
		if host, ok := originHost(origin); !ok || !hostNamesServer(host, ip, allowedHosts) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Origin header %q; a name the server is known by is allowed with WithAllowedHosts", origin), http.StatusForbidden)
			return true
		}
	}
	return false
}

package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// isLoopbackHost reports whether addr refers to a loopback interface. addr
// may be a bare host ("localhost", "127.0.0.1", "::1", "[::1]") or a
// host:port pair ("localhost:3000", "127.0.0.1:3000", "[::1]:3000"). The
// name compares by ASCII case only and without one trailing dot, as a fully
// qualified "localhost." names the same host; "localhost.." does not.
//
// It is part of DNS rebinding protection: a request that arrives on a
// connection a rebinding attack can reach must name the server, otherwise a
// malicious website could rebind its own domain to the server's address and
// drive a victim's browser to interact with it.
func isLoopbackHost(addr string) bool {
	host := hostName(addr)
	if equalFoldASCII(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	if !mayBeIP(host) {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}

// allowedHostEntry is entry as WithAllowedHosts keeps it: without the
// whitespace around it, and a URL's host when it is one, as in
// "https://mcp.example.com:8443/mcp". ok is false for an entry that names
// no host: an empty or blank one, one with only a port, a lone dot, or a URL
// without a host. A URL that does not parse is kept as written, so it still
// turns the Host check on and names nothing a request sends.
func allowedHostEntry(entry string) (host string, ok bool) {
	entry = strings.TrimSpace(entry)
	if strings.Contains(entry, "://") {
		if u, err := url.Parse(entry); err == nil {
			entry = u.Host
		}
	}
	return entry, strings.TrimSuffix(hostName(entry), ".") != ""
}

// hostName is host without its port and the brackets of an IPv6 literal. A
// host without a colon has no port, and is not handed to net.SplitHostPort,
// which would only allocate its error.
func hostName(host string) string {
	if strings.IndexByte(host, ':') < 0 {
		return strings.Trim(host, "[]")
	}
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
	if !ok || (!equalFoldASCII(scheme, "http") && !equalFoldASCII(scheme, "https")) {
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
// Names are compared without their port, by ASCII case only and without a
// trailing dot; an IP literal is compared as an address without its zone,
// so "[::1]" and "::1" are one, as are "[fe80::1%25en0]" and "fe80::1".
func hostNamesServer(host string, ip netip.Addr, allowed []string) bool {
	if isLoopbackHost(host) {
		return true
	}
	name := hostName(host)
	var hostIP netip.Addr
	isIP := errNotIP
	if mayBeIP(name) {
		hostIP, isIP = netip.ParseAddr(name)
	}
	if isIP == nil {
		hostIP = hostIP.Unmap().WithZone("")
		if hostIP == ip {
			return true
		}
	}
	for _, entry := range allowed {
		entryName := hostName(entry)
		if isIP == nil {
			if !mayBeIP(entryName) {
				continue
			}
			if entryIP, err := netip.ParseAddr(entryName); err == nil && entryIP.Unmap().WithZone("") == hostIP {
				return true
			}
			continue
		}
		if equalFoldASCII(strings.TrimSuffix(name, "."), strings.TrimSuffix(entryName, ".")) {
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
// and, once allowedHosts names hosts, on every request whatever its address,
// is one whose Host header does not; then it returns true. Otherwise it
// returns false and writes nothing. checkOrigin false leaves Origin to a
// configured CORS allow list, the authority on it then.
//
// Only a browser can be rebound, and a browser sends Origin on every POST,
// so Origin catches the rebound request on any address: what the browser
// reports about the page that sent it, which rebinding cannot change. Host
// names the rebound domain too, but on a private address it names the
// server's own hostname as well, as a reverse proxy or a container network
// forwards it, so without a list Host is checked only on a loopback
// address, where the local-server guidance asks for it. The server
// validates Origin on every connection and answers an invalid one with 403
// (Transports.md row 14).
//
// With allowedHosts naming hosts, Host is checked on every request, on any
// address or none known: a public address only the victim's network
// reaches, such as a firewalled global IPv6 address, is a rebinding target
// too, and every legitimate client names the server in Host, so the list
// turns none away. Without a list a request on a public address is not
// checked: the attacker reaches such a server without rebinding.
//
// See https://modelcontextprotocol.io/specification/2025-11-25/basic/security_best_practices#local-mcp-server-compromise
func rejectDNSRebinding(w http.ResponseWriter, r *http.Request, checkOrigin bool, allowedHosts []string) bool {
	// ip stays the zero address when the connection's address is unknown,
	// which names no server: only a loopback name or a listed host does.
	var ip netip.Addr
	target := false
	if localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && localAddr != nil {
		if addr, ok := localAddrIP(localAddr); ok {
			ip, target = addr, isRebindingTarget(addr)
		}
	}
	if (len(allowedHosts) > 0 || (target && ip.IsLoopback())) && !hostNamesServer(r.Host, ip, allowedHosts) {
		http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q; a name the server is known by is allowed with WithAllowedHosts", r.Host), http.StatusForbidden)
		return true
	}
	if !target {
		return false
	}
	if origin := r.Header.Get("Origin"); checkOrigin && origin != "" {
		if host, ok := originHost(origin); !ok || !hostNamesServer(host, ip, allowedHosts) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Origin header %q; a name the server is known by is allowed with WithAllowedHosts", origin), http.StatusForbidden)
			return true
		}
	}
	return false
}

// equalFoldASCII reports whether a and b are equal once ASCII letters are
// lowered, every other byte matching exactly. Host names and schemes compare
// this way (RFC 3986 sections 3.1 and 3.2.2): Unicode case folding, as in
// strings.EqualFold, takes some distinct hosts for one, such as a long s for
// an s, or names with a final and a medial sigma.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

// lowerASCII is c with A to Z lowered, and any other byte as it is.
func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// errNotIP stands for ParseAddr's error on a name mayBeIP rules out, which
// is not asked to parse it: its error would be allocated on every request.
var errNotIP = errors.New("not an IP literal")

// mayBeIP is false only for a name netip.ParseAddr refuses: an IPv4 literal
// starts with a digit and an IPv6 one holds a colon.
func mayBeIP(s string) bool {
	return s != "" && ('0' <= s[0] && s[0] <= '9' || strings.IndexByte(s, ':') >= 0)
}

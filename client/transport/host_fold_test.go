package transport

import (
	"net/url"
	"testing"
)

// Host names compare case-insensitively in ASCII only (RFC 3986 section
// 3.2.2): Unicode case folding takes some distinct hosts for one, such as
// names with a final and a medial sigma, the Kelvin sign and k, or a long s
// and s, and url.Parse maps none of them through IDNA. An IPv6 zone names
// an interface, and interface names can differ by case alone, so a zone
// compares exactly. Every client check that compares hosts holds to that,
// in either argument order, with or without a port.
func TestHostsCompareByASCIICaseOnly(t *testing.T) {
	hosts := []struct {
		a, b  string
		equal bool
	}{
		{"xσ.example", "xς.example", false},    // a medial and a final sigma
		{"\u212A.example", "k.example", false}, // the Kelvin sign
		{"localhoſt", "localhost", false},      // a long s
		{"MCP.example", "mcp.example", true},
		{"mcp.example", "MCP.example", true},
		{"mcp.exampl", "mcp.example", false},
		{"mcp.example", "mcp.exampl", false},
		{"[fe80::1%25eth0]", "[fe80::1%25ETH0]", false},
		{"[fe80::1%25ETH0]", "[fe80::1%25eth0]", false},
		{"[fe80::1%25eth0]:8443", "[fe80::1%25ETH0]:8443", false},
		{"[FE80::A%25eth0]", "[fe80::a%25eth0]", true},
		{"[fe80::a%25eth0]:8443", "[FE80::A%25eth0]:8443", true},
	}
	parse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return u
	}
	sites := []struct {
		name  string
		equal func(a, b string) bool
	}{
		{"sameOrigin", func(a, b string) bool {
			return sameOrigin(parse("https://"+a+"/mcp"), parse("https://"+b+"/mcp"))
		}},
		{"checkAdvertisedPRMURL", func(a, b string) bool {
			return checkAdvertisedPRMURL("https://"+a+"/.well-known/oauth-protected-resource", "https://"+b+"/mcp") == nil
		}},
		{"resourceComponentsEqual", func(a, b string) bool {
			return resourceComponentsEqual(parse("https://"+a+"/mcp"), parse("https://"+b+"/mcp"))
		}},
	}
	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			for _, h := range hosts {
				if got := site.equal(h.a, h.b); got != h.equal {
					t.Errorf("%s(%s, %s) = %v, want %v", site.name, h.a, h.b, got, h.equal)
				}
			}
		})
	}

	t.Run("ValidateRedirectURI", func(t *testing.T) {
		uris := []struct {
			uri string
			ok  bool
		}{
			{"http://localhoſt:8085/cb", false},
			{"http://LOCALHOST:8085/cb", true},
			{"http://localhost:8085/cb", true},
			{"http://localhos:8085/cb", false},
		}
		for _, u := range uris {
			if err := ValidateRedirectURI(u.uri); (err == nil) != u.ok {
				t.Errorf("ValidateRedirectURI(%s) = %v, want ok %v", u.uri, err, u.ok)
			}
		}
	})
}

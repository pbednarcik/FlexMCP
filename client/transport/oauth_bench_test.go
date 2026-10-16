package transport

import (
	"testing"
	"time"
)

// benchOAuthHandler returns a handler for base holding a valid token, with
// discovery's results in place.
func benchOAuthHandler(b *testing.B, base string) *OAuthHandler {
	b.Helper()
	store := NewMemoryTokenStore()
	if err := store.SaveToken(b.Context(), &Token{
		AccessToken:  "access",
		TokenType:    "bearer",
		RefreshToken: "refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour),
		Resource:     base,
		Issuer:       "https://auth.example.com",
	}); err != nil {
		b.Fatal(err)
	}
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", TokenStore: store})
	handler.SetBaseURL(base)
	handler.metadataMu.Lock()
	handler.resourceURL = base
	handler.knownIssuer = "https://auth.example.com"
	handler.metadataMu.Unlock()
	return handler
}

// BenchmarkOAuthAuthorizationHeader is the per-request cost of the header an
// OAuth transport attaches: the stored token read, checked against the server
// discovery named, and formatted.
func BenchmarkOAuthAuthorizationHeader(b *testing.B) {
	const base = "https://mcp.example.com/mcp"
	handler := benchOAuthHandler(b, base)

	b.ReportAllocs()
	for b.Loop() {
		if _, err := handler.authorizationHeader(b.Context(), base); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOAuthAuthorizationHeaderParallel is the same header asked for from
// every P at once, as concurrent requests of one client ask for it.
func BenchmarkOAuthAuthorizationHeaderParallel(b *testing.B) {
	const base = "https://mcp.example.com/mcp"
	handler := benchOAuthHandler(b, base)
	ctx := b.Context()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := handler.authorizationHeader(ctx, base); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

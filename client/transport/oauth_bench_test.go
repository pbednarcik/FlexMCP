package transport

import (
	"testing"
	"time"
)

// BenchmarkOAuthAuthorizationHeader is the per-request cost of the header an
// OAuth transport attaches: the stored token read, checked against the server
// discovery named, and formatted.
func BenchmarkOAuthAuthorizationHeader(b *testing.B) {
	const base = "https://mcp.example.com/mcp"
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

	b.ReportAllocs()
	for b.Loop() {
		if _, err := handler.authorizationHeader(b.Context(), base); err != nil {
			b.Fatal(err)
		}
	}
}

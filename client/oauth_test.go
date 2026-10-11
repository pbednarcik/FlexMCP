package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOAuthSSEClientAuthorizesEveryRequest(t *testing.T) {
	srv := newLegacySSEServer(t)
	tokenStore := NewMemoryTokenStore()
	require.NoError(t, tokenStore.SaveToken(t.Context(), &Token{
		AccessToken: "sse-token",
		TokenType:   "Bearer",
		ExpiresIn:   3600,
		ExpiresAt:   time.Now().Add(time.Hour),
	}))
	c, err := NewOAuthSSEClient(srv.URL+"/sse", OAuthConfig{
		ClientID:    "test-client",
		RedirectURI: "http://localhost:8085/callback",
		Scopes:      []string{"mcp.read"},
		TokenStore:  tokenStore,
	})
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	sse, ok := c.GetTransport().(*transport.SSE)
	require.True(t, ok, "expected an SSE transport, got %T", c.GetTransport())
	assert.True(t, sse.IsOAuthEnabled())
	headers := srv.postHeaders()
	require.NotEmpty(t, headers)
	for _, h := range headers {
		assert.Equal(t, "Bearer sse-token", h.Get("Authorization"))
	}
}

func TestHTTPClientConstructorsRejectAnInvalidURL(t *testing.T) {
	config := OAuthConfig{ClientID: "test-client"}
	tests := []struct {
		name      string
		construct func(url string) (*Client, error)
	}{
		{name: "streamable HTTP", construct: func(url string) (*Client, error) { return NewStreamableHttpClient(url) }},
		{name: "streamable HTTP with OAuth", construct: func(url string) (*Client, error) { return NewOAuthStreamableHttpClient(url, config) }},
		{name: "SSE with OAuth", construct: func(url string) (*Client, error) { return NewOAuthSSEClient(url, config) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.construct("://not a url")
			require.Error(t, err)
			assert.Nil(t, c)
		})
	}
}

func TestNewOAuthStreamableHttpClient(t *testing.T) {
	ctx := t.Context()
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Return a successful response
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo": map[string]any{
					"name":    "test-server",
					"version": "1.0.0",
				},
				"capabilities": map[string]any{},
			},
		}); err != nil {
			t.Errorf("Failed to encode JSON response: %v", err)
		}
	}))
	defer server.Close()

	// Create a token store with a valid token
	tokenStore := NewMemoryTokenStore()
	validToken := &Token{
		AccessToken:  "test-token",
		TokenType:    "Bearer",
		RefreshToken: "refresh-token",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(1 * time.Hour), // Valid for 1 hour
	}
	if err := tokenStore.SaveToken(ctx, validToken); err != nil {
		t.Fatalf("Failed to save token: %v", err)
	}

	// Create OAuth config
	oauthConfig := OAuthConfig{
		ClientID:    "test-client",
		RedirectURI: "http://localhost:8085/callback",
		Scopes:      []string{"mcp.read", "mcp.write"},
		TokenStore:  tokenStore,
	}

	// Create client with OAuth
	client, err := NewOAuthStreamableHttpClient(server.URL, oauthConfig)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Start the client
	if err := client.Start(t.Context()); err != nil {
		t.Fatalf("Failed to start client: %v", err)
	}
	defer client.Close()

	// Verify that the client was created successfully
	trans := client.GetTransport()
	streamableHTTP, ok := trans.(*transport.StreamableHTTP)
	if !ok {
		t.Fatalf("Expected transport to be *transport.StreamableHTTP, got %T", trans)
	}

	// Verify OAuth is enabled
	if !streamableHTTP.IsOAuthEnabled() {
		t.Errorf("Expected IsOAuthEnabled() to return true")
	}

	// Verify the OAuth handler is set
	if streamableHTTP.GetOAuthHandler() == nil {
		t.Errorf("Expected GetOAuthHandler() to return a handler")
	}
}

func TestIsOAuthAuthorizationRequiredError(t *testing.T) {
	// Create a test error
	err := &transport.OAuthAuthorizationRequiredError{
		Handler: transport.NewOAuthHandler(transport.OAuthConfig{}),
	}

	// Verify IsOAuthAuthorizationRequiredError returns true
	if !IsOAuthAuthorizationRequiredError(err) {
		t.Errorf("Expected IsOAuthAuthorizationRequiredError to return true")
	}

	// Verify GetOAuthHandler returns the handler
	handler := GetOAuthHandler(err)
	if handler == nil {
		t.Errorf("Expected GetOAuthHandler to return a handler")
	}

	// Test with a different error
	err2 := fmt.Errorf("some other error")

	// Verify IsOAuthAuthorizationRequiredError returns false
	if IsOAuthAuthorizationRequiredError(err2) {
		t.Errorf("Expected IsOAuthAuthorizationRequiredError to return false")
	}

	// Verify GetOAuthHandler returns nil
	handler = GetOAuthHandler(err2)
	if handler != nil {
		t.Errorf("Expected GetOAuthHandler to return nil")
	}
}

func TestGetResourceMetadataURL(t *testing.T) {
	// Test with error containing metadata URL
	metadataURL := "https://auth.example.com/.well-known/oauth-protected-resource"
	err := &transport.OAuthAuthorizationRequiredError{
		Handler:             transport.NewOAuthHandler(transport.OAuthConfig{}),
		ResourceMetadataURL: metadataURL,
	}

	// Verify GetResourceMetadataURL returns the correct URL
	result := GetResourceMetadataURL(err)
	if result != metadataURL {
		t.Errorf("Expected GetResourceMetadataURL to return %q, got %q", metadataURL, result)
	}

	// Test with error containing no metadata URL
	err2 := &transport.OAuthAuthorizationRequiredError{
		Handler:             transport.NewOAuthHandler(transport.OAuthConfig{}),
		ResourceMetadataURL: "",
	}

	result2 := GetResourceMetadataURL(err2)
	if result2 != "" {
		t.Errorf("Expected GetResourceMetadataURL to return empty string, got %q", result2)
	}

	// Test with non-OAuth error
	err3 := fmt.Errorf("some other error")

	result3 := GetResourceMetadataURL(err3)
	if result3 != "" {
		t.Errorf("Expected GetResourceMetadataURL to return empty string for non-OAuth error, got %q", result3)
	}
}

func TestIsAuthorizationRequiredError(t *testing.T) {
	// Test with base AuthorizationRequiredError (401 without OAuth handler)
	metadataURL := "https://auth.example.com/.well-known/oauth-protected-resource"
	err := &transport.AuthorizationRequiredError{
		ResourceMetadataURL: metadataURL,
	}

	// Verify IsAuthorizationRequiredError returns true
	if !IsAuthorizationRequiredError(err) {
		t.Errorf("Expected IsAuthorizationRequiredError to return true for AuthorizationRequiredError")
	}

	// Verify GetResourceMetadataURL returns the correct URL
	result := GetResourceMetadataURL(err)
	if result != metadataURL {
		t.Errorf("Expected GetResourceMetadataURL to return %q, got %q", metadataURL, result)
	}

	// Test with OAuthAuthorizationRequiredError (different type)
	oauthErr := &transport.OAuthAuthorizationRequiredError{
		Handler:             transport.NewOAuthHandler(transport.OAuthConfig{}),
		ResourceMetadataURL: metadataURL,
	}

	// Verify IsOAuthAuthorizationRequiredError returns true
	if !IsOAuthAuthorizationRequiredError(oauthErr) {
		t.Errorf("Expected IsOAuthAuthorizationRequiredError to return true for OAuthAuthorizationRequiredError")
	}

	// Verify GetResourceMetadataURL works with OAuth error too
	result2 := GetResourceMetadataURL(oauthErr)
	if result2 != metadataURL {
		t.Errorf("Expected GetResourceMetadataURL to return %q, got %q", metadataURL, result2)
	}

	// Test with non-authorization error
	err3 := fmt.Errorf("some other error")

	// Verify IsAuthorizationRequiredError returns false
	if IsAuthorizationRequiredError(err3) {
		t.Errorf("Expected IsAuthorizationRequiredError to return false for non-authorization error")
	}
}

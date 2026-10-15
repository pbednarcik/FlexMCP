package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A redirect URI has a host and no fragment (RFC 6749 section 3.1.2) and no
// credentials; a loopback host is accepted in any letter case and on any
// loopback address (RFC 8252 section 7.3).
func TestValidateRedirectURIIsStrictAboutShape(t *testing.T) {
	for _, uri := range []string{
		"https://",
		"https:///callback",
		"https://app.example/callback#fragment",
		"https://user:secret@app.example/callback",
		"http://localhost:8080/callback#fragment",
	} {
		assert.Error(t, ValidateRedirectURI(uri), uri)
	}
	for _, uri := range []string{
		"http://LOCALHOST:8080/callback",
		"http://127.0.0.2:8080/callback",
		"http://[::1]:8080/callback",
	} {
		assert.NoError(t, ValidateRedirectURI(uri), uri)
	}
}

// A random string of no length is an error, not a panic.
func TestGenerateRandomStringRefusesANonPositiveLength(t *testing.T) {
	for _, length := range []int{0, -1} {
		_, err := GenerateRandomString(length)
		assert.Error(t, err, "length %d", length)
	}
}

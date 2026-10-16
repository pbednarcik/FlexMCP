package transport

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// GenerateRandomString generates a random string of the specified length,
// which is positive.
func GenerateRandomString(length int) (string, error) {
	if length <= 0 {
		return "", errors.New("random string length must be positive")
	}
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes)[:length], nil
}

// GenerateCodeVerifier generates a code verifier for PKCE
func GenerateCodeVerifier() (string, error) {
	// According to RFC 7636, the code verifier should be between 43 and 128 characters
	return GenerateRandomString(64)
}

// GenerateCodeChallenge generates a code challenge from a code verifier
func GenerateCodeChallenge(codeVerifier string) string {
	// SHA256 hash the code verifier
	hash := sha256.Sum256([]byte(codeVerifier))
	// Base64url encode the hash
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

// GenerateState generates a state parameter for OAuth
func GenerateState() (string, error) {
	return GenerateRandomString(32)
}

// ValidateRedirectURI validates that a redirect URI is secure: an HTTPS URL,
// or an HTTP URL on a loopback host (RFC 8252 section 7.3), in either case
// with a host, without credentials and without a fragment (RFC 6749 section
// 3.1.2). This helper is advisory: the OAuth handler does not call it.
func ValidateRedirectURI(redirectURI string) error {
	if redirectURI == "" {
		return errors.New("redirect URI cannot be empty")
	}

	parsedURL, err := url.Parse(redirectURI)
	if err != nil {
		return fmt.Errorf("invalid redirect URI: %w", err)
	}
	hostname := parsedURL.Hostname()
	if hostname == "" {
		return errors.New("redirect URI must have a host")
	}
	if parsedURL.User != nil {
		return errors.New("redirect URI must not carry credentials")
	}
	if parsedURL.Fragment != "" || strings.HasSuffix(redirectURI, "#") {
		return errors.New("redirect URI must not have a fragment")
	}

	switch parsedURL.Scheme {
	case "https":
		return nil
	case "http":
		if strings.EqualFold(hostname, "localhost") {
			return nil
		}
		if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("HTTP redirect URI must use a loopback host")
	}
	return errors.New("redirect URI must use either HTTP on a loopback host or HTTPS")
}

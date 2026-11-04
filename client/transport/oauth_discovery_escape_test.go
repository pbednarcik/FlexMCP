package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The discovery URLs keep the issuer path's escapes, as buildWellKnownURL does
// for the resource: an escaped slash names one segment, so turning it into a
// real one asks another path.
func TestAuthorizationServerMetadataURLs_KeepIssuerEscapes(t *testing.T) {
	tests := []struct {
		name   string
		issuer string
		want   []string
	}{
		{
			name:   "plain path",
			issuer: "https://auth.example.com/tenant",
			want: []string{
				"https://auth.example.com/.well-known/oauth-authorization-server/tenant",
				"https://auth.example.com/.well-known/openid-configuration/tenant",
				"https://auth.example.com/tenant/.well-known/openid-configuration",
			},
		},
		{
			name:   "escaped slash",
			issuer: "https://auth.example.com/tenant%2Fx",
			want: []string{
				"https://auth.example.com/.well-known/oauth-authorization-server/tenant%2Fx",
				"https://auth.example.com/.well-known/openid-configuration/tenant%2Fx",
				"https://auth.example.com/tenant%2Fx/.well-known/openid-configuration",
			},
		},
		{
			name:   "escaped space",
			issuer: "https://auth.example.com/tenant%20x",
			want: []string{
				"https://auth.example.com/.well-known/oauth-authorization-server/tenant%20x",
				"https://auth.example.com/.well-known/openid-configuration/tenant%20x",
				"https://auth.example.com/tenant%20x/.well-known/openid-configuration",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, authorizationServerMetadataURLs(tt.issuer))
		})
	}
}

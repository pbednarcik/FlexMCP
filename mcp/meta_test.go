package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaServerInfoAndSubscriptionIDSurviveTheWire(t *testing.T) {
	original := &Meta{}
	original.SetServerInfo(Implementation{Name: "srv", Version: "3.1"})
	original.SetSubscriptionID(int64(42))
	require.NotNil(t, original.ServerInfo())
	assert.Equal(t, "srv", original.ServerInfo().Name)
	assert.Equal(t, int64(42), original.SubscriptionID())

	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"io.modelcontextprotocol/serverInfo": {"name": "srv", "version": "3.1"},
		"io.modelcontextprotocol/subscriptionId": 42
	}`, string(encoded))

	var decoded Meta
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NotNil(t, decoded.ServerInfo())
	assert.Equal(t, Implementation{Name: "srv", Version: "3.1"}, *decoded.ServerInfo())

	// The subscription id is the JSON-RPC id of the listen request, handed
	// back as decoded: a JSON number arrives as float64.
	assert.Equal(t, float64(42), decoded.SubscriptionID())
	assert.Equal(t, float64(42), decoded.GetMetaField(MetaKeySubscriptionID))
}

func TestMetaAccessorsIgnoreMalformedValues(t *testing.T) {
	var decoded Meta
	require.NoError(t, json.Unmarshal([]byte(`{
		"io.modelcontextprotocol/serverInfo": "not-an-object",
		"io.modelcontextprotocol/clientInfo": {"name": "client", "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": [1],
		"io.modelcontextprotocol/protocolVersion": null,
		"io.modelcontextprotocol/logLevel": 3
	}`), &decoded))

	assert.Nil(t, decoded.ServerInfo())
	assert.Nil(t, decoded.ClientCapabilities())
	assert.Empty(t, decoded.ProtocolVersion())
	assert.Empty(t, decoded.LogLevel())
	assert.Nil(t, decoded.SubscriptionID())

	require.NotNil(t, decoded.ClientInfo(), "a well-formed sibling is unaffected")
	assert.Equal(t, "client", decoded.ClientInfo().Name)

	assert.Nil(t, (&Meta{}).GetMetaField(MetaKeyServerInfo))
	var none *Meta
	assert.Nil(t, none.GetMetaField(MetaKeyServerInfo))
}

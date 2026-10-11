package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResultMetadataDecoratesAnyResult(t *testing.T) {
	// A transport decorates a result without knowing its concrete type, and
	// what it set must reach the wire.
	results := []struct {
		name   string
		result ResultMetadata
	}{
		{"tool call", &CallToolResult{}},
		{"tool list", &ListToolsResult{}},
		{"task", &GetTaskResult{}},
		{"discover", &DiscoverResult{}},
		{"initialize", &InitializeResult{}},
	}

	for _, tt := range results {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, ResultType(""), tt.result.GetResultType())
			assert.Nil(t, tt.result.GetResultMeta())

			tt.result.SetResultType(ResultTypeComplete)
			tt.result.EnsureResultMeta().SetServerInfo(Implementation{Name: "srv", Version: "1"})
			assert.Same(t, tt.result.GetResultMeta(), tt.result.EnsureResultMeta(), "allocated once")
			assert.Equal(t, ResultTypeComplete, tt.result.GetResultType())

			encoded, err := json.Marshal(tt.result)
			require.NoError(t, err)

			var wire struct {
				ResultType ResultType `json:"resultType"`
				Meta       *Meta      `json:"_meta"`
			}
			require.NoError(t, json.Unmarshal(encoded, &wire))
			assert.Equal(t, ResultTypeComplete, wire.ResultType)
			require.NotNil(t, wire.Meta.ServerInfo())
			assert.Equal(t, "srv", wire.Meta.ServerInfo().Name)

			tt.result.SetResultMeta(nil)
			assert.Nil(t, tt.result.GetResultMeta())
		})
	}
}

func TestResultMetadataOnNilReceiver(t *testing.T) {
	var result *Result
	result.SetResultType(ResultTypeComplete)
	result.SetResultMeta(&Meta{})
	assert.Equal(t, ResultType(""), result.GetResultType())
	assert.Nil(t, result.GetResultMeta())
	assert.Nil(t, result.EnsureResultMeta())

	var cacheable *CacheableResult
	cacheable.SetCacheHints(10, CacheScopePublic)
	_, ok := cacheable.TTL()
	assert.False(t, ok)
}

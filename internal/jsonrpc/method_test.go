package jsonrpc

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMethodRecordsPresenceAndType(t *testing.T) {
	for message, want := range map[string]Method{
		`{}`:                  {},
		`{"method":"ping"}`:   {Present: true, IsString: true, Name: "ping"},
		`{"method":""}`:       {Present: true, IsString: true},
		`{"method":null}`:     {Present: true},
		`{"method":5}`:        {Present: true},
		`{"method":["ping"]}`: {Present: true},
		`{"method":"tools"}`:  {Present: true, IsString: true, Name: "tools"},
		`{"method":"café"}`:   {Present: true, IsString: true, Name: "café"},
	} {
		var envelope struct {
			Method Method `json:"method"`
		}
		require.NoError(t, json.Unmarshal([]byte(message), &envelope), message)
		assert.Equal(t, want, envelope.Method, message)
	}
}

func TestMethodOfLooksAgainOnlyAtAnEmptyName(t *testing.T) {
	assert.Equal(t, Method{Present: true, IsString: true, Name: "ping"}, MethodOf("ping", nil))
	for message, want := range map[string]Method{
		`{"id":1}`:               {},
		`{"id":1,"method":""}`:   {Present: true, IsString: true},
		`{"id":1,"method":null}`: {Present: true},
	} {
		assert.Equal(t, want, MethodOf("", []byte(message)), message)
	}
}

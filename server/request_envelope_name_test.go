package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModernProtocol_TaskMethodsNameTheTaskInMcpName: a 2026-07-28 HTTP
// request of a Tasks extension method whose Mcp-Name is its task id passes
// the header check, whatever answer the method then gets.
func TestModernProtocol_TaskMethodsNameTheTaskInMcpName(t *testing.T) {
	srv := newModernTestServer(t)

	for _, method := range []mcp.MCPMethod{mcp.MethodTasksGet, mcp.MethodTasksCancel, mcp.MethodTasksUpdate} {
		t.Run(string(method), func(t *testing.T) {
			resp := postModern(t, srv.URL, method, map[string]any{"taskId": "abc"})
			if errDetails, ok := decodeJSONRPC(t, resp)["error"].(map[string]any); ok {
				assert.NotEqual(t, float64(mcp.HEADER_MISMATCH), errDetails["code"],
					"a matching Mcp-Name is no header mismatch: %v", errDetails)
			}
		})

		t.Run(string(method)+" with another task id in Mcp-Name", func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": method,
				"params": map[string]any{"taskId": "abc", "_meta": modernMeta()},
			})
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
			req.Header.Set(mcp.HeaderMethod, string(method))
			req.Header.Set(mcp.HeaderName, "other")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			errDetails, ok := decodeJSONRPC(t, resp)["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, float64(mcp.HEADER_MISMATCH), errDetails["code"])
		})
	}
}

// TestParamsPeekHeaderNameAgreesWithExtractHeaderName keeps the server's read
// of the Mcp-Name value in step with [mcp.ExtractHeaderName], for every method
// either knows and for params that omit the member or give it another type.
func TestParamsPeekHeaderNameAgreesWithExtractHeaderName(t *testing.T) {
	methods := []mcp.MCPMethod{
		mcp.MethodToolsCall, mcp.MethodPromptsGet, mcp.MethodResourcesRead,
		mcp.MethodTasksGet, mcp.MethodTasksCancel, mcp.MethodTasksUpdate,
		mcp.MethodToolsList, mcp.MethodTasksList, mcp.MethodTasksResult,
	}
	params := []string{
		`{}`,
		`{"name":"n"}`,
		`{"uri":"file:///a"}`,
		`{"taskId":"abc"}`,
		`{"taskId":""}`,
		`{"taskId":null}`,
		`{"taskId":5}`,
		`{"taskId":{"a":1}}`,
		`{"name":"n","uri":"u","taskId":"t"}`,
	}

	var named int
	for _, method := range methods {
		if mcp.MethodRequiresNameHeader(method) {
			named++
		}
		for _, p := range params {
			t.Run(string(method)+" "+p, func(t *testing.T) {
				var envelope requestEnvelope
				require.NoError(t, readEnvelope([]byte(`{"jsonrpc":"2.0","id":1,"method":"`+string(method)+`","params":`+p+`}`), &envelope))

				wantName, wantOK := mcp.ExtractHeaderName(method, json.RawMessage(p))
				gotName, gotOK := envelope.Params.headerName(method)
				assert.Equal(t, wantOK, gotOK)
				assert.Equal(t, wantName, gotName)
			})
		}
	}
	assert.Equal(t, 6, named, "the methods that carry a name in Mcp-Name")
}

package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestIdRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		id    RequestId
		wire  string
		value any
		text  string
	}{
		{"string", NewRequestId("abc"), `"abc"`, "abc", "string:abc"},
		{"integer", NewRequestId(int64(7)), `7`, int64(7), "int64:7"},
		{"integral float", NewRequestId(float64(3)), `3`, int64(3), "int64:3"},
		{"integer past 2^53", NewRequestId(int64(9007199254740993)), `9007199254740993`, int64(9007199254740993), "int64:9007199254740993"},
		{"negative past 2^53", NewRequestId(int64(-9007199254740993)), `-9007199254740993`, int64(-9007199254740993), "int64:-9007199254740993"},
		{"integer past int64", NewRequestId(json.Number("9223372036854775808")), `9223372036854775808`, json.Number("9223372036854775808"), "number:9223372036854775808"},
		{"negative past int64", NewRequestId(json.Number("-9223372036854775809")), `-9223372036854775809`, json.Number("-9223372036854775809"), "number:-9223372036854775809"},
		{"null", NewRequestId(nil), `null`, nil, "<nil>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := JSONRPCRequest{JSONRPC: JSONRPC_VERSION, ID: tt.id, Method: "ping"}
			encoded, err := json.Marshal(request)
			require.NoError(t, err)
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":`+tt.wire+`,"method":"ping"}`, string(encoded))
			assert.Contains(t, string(encoded), `"id":`+tt.wire+`,`, "JSONEq compares numbers as float64")

			var decoded JSONRPCRequest
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			assert.Equal(t, tt.value, decoded.ID.Value())
			assert.Equal(t, tt.text, decoded.ID.String())
			assert.Equal(t, tt.value == nil, decoded.ID.IsNil())
		})
	}
}

// A request id is a string or an integer (MCP base protocol), and an integer
// is a plain literal: one with a fraction or an exponent is refused, even when
// its value is whole, so no id passes through a float64 and none depends on
// its spelling or its size.
func TestRequestIdDecodesOnlyIntegerLiterals(t *testing.T) {
	for wire, value := range map[string]any{
		`0`:                    int64(0),
		`-0`:                   int64(0),
		`-7`:                   int64(-7),
		`18446744073709551616`: json.Number("18446744073709551616"),
		`"1.5"`:                "1.5",
	} {
		var id RequestId
		require.NoError(t, json.Unmarshal([]byte(wire), &id), wire)
		assert.Equal(t, value, id.Value(), wire)
	}
	for _, wire := range []string{`1.5`, `1.0`, `1e3`, `1E3`, `-0.0`, `9.223372036854775808e18`, `1e19`, `1e400`} {
		var id RequestId
		require.EqualError(t, json.Unmarshal([]byte(wire), &id), "invalid request id: "+wire)
	}
}

// An id built in process from any Go integer type is the same id as the one
// the wire gives back, an int64 or a json.Number past it, so a request made
// with NewRequestId(1) is matched to the response that carries 1.
func TestRequestIdStringIsTheSameForEveryIntegerType(t *testing.T) {
	for _, value := range []any{1, int8(1), int16(1), int32(1), int64(1), uint(1), uint8(1), uint16(1), uint32(1), uint64(1), json.Number("1")} {
		assert.Equal(t, "int64:1", NewRequestId(value).String(), "%T", value)
	}
	assert.Equal(t, "number:18446744073709551615", NewRequestId(uint64(18446744073709551615)).String())
	var decoded RequestId
	require.NoError(t, json.Unmarshal([]byte(`18446744073709551615`), &decoded))
	assert.Equal(t, decoded.String(), NewRequestId(uint64(18446744073709551615)).String())
}

func TestRequestIdRefusesNonScalarIDs(t *testing.T) {
	var id RequestId
	require.EqualError(t, json.Unmarshal([]byte(`true`), &id), "invalid request id: true")
	require.EqualError(t, json.Unmarshal([]byte(`{"a":1}`), &id), `invalid request id: {"a":1}`)

	// An id built in process from an unsupported type still prints.
	assert.Equal(t, "unknown:true", NewRequestId(true).String())
}

func TestNotificationParamsRoundTrip(t *testing.T) {
	notification := Notification{
		Method: "notifications/resources/updated",
		Params: NotificationParams{
			Meta: map[string]any{"progressToken": "p1"},
			AdditionalFields: map[string]any{
				"uri":   "file:///a",
				"_meta": map[string]any{"progressToken": "smuggled"},
			},
		},
	}

	encoded, err := json.Marshal(notification)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"method": "notifications/resources/updated",
		"params": {"_meta": {"progressToken": "p1"}, "uri": "file:///a"}
	}`, string(encoded), "the typed _meta wins over one smuggled in as an additional field")

	var decoded Notification
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, map[string]any{"progressToken": "p1"}, decoded.Params.Meta)
	assert.Equal(t, map[string]any{"uri": "file:///a"}, decoded.Params.AdditionalFields)

	// A _meta that is not an object is dropped rather than failing the
	// notification; anything else malformed is refused.
	var lenient NotificationParams
	require.NoError(t, json.Unmarshal([]byte(`{"_meta": 1, "uri": "file:///b"}`), &lenient))
	assert.Empty(t, lenient.Meta)
	assert.Equal(t, map[string]any{"uri": "file:///b"}, lenient.AdditionalFields)
	assert.Error(t, json.Unmarshal([]byte(`[]`), &lenient))
}

func TestLoggingLevelShouldSendTo(t *testing.T) {
	tests := []struct {
		name  string
		level LoggingLevel
		floor LoggingLevel
		want  bool
	}{
		{"more severe than the floor", LoggingLevelError, LoggingLevelInfo, true},
		{"at the floor", LoggingLevelInfo, LoggingLevelInfo, true},
		{"less severe than the floor", LoggingLevelDebug, LoggingLevelInfo, false},
		{"unknown message level", LoggingLevel("verbose"), LoggingLevelDebug, false},
		{"unknown floor", LoggingLevelEmergency, LoggingLevel(""), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.level.ShouldSendTo(tt.floor))
		})
	}
}

func TestContentVariantsRoundTrip(t *testing.T) {
	size := int64(12)
	contents := []Content{
		NewTextContent("hi"),
		NewImageContent("aGk=", "image/png"),
		NewAudioContent("aGk=", "audio/wav"),
		ResourceLink{Type: ContentTypeLink, URI: "file:///a", Name: "a", MIMEType: "text/plain", Size: &size},
		NewEmbeddedResource(TextResourceContents{URI: "file:///a", Text: "x"}),
		NewEmbeddedResource(BlobResourceContents{URI: "file:///b", Blob: "aGk="}),
		ToolUseContent{Type: ContentTypeToolUse, ID: "1", Name: "t", Input: map[string]any{"k": "v"}},
		ToolResultContent{Type: ContentTypeToolResult, ToolUseID: "1", Content: []Content{NewTextContent("r")}},
	}

	for _, content := range contents {
		encoded, err := MarshalContent(content)
		require.NoError(t, err)

		decoded, err := UnmarshalContent(encoded)
		require.NoError(t, err, string(encoded))
		assert.Equal(t, content, decoded, string(encoded))
	}
}

func TestUnmarshalContentRefusesBadPayloads(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"not json", `{`, "unexpected end of JSON input"},
		{"no type", `{"text":"hi"}`, "missing or invalid type field"},
		{"type not a string", `{"type":1,"text":"hi"}`, "missing or invalid type field"},
		{"unknown type", `{"type":"video"}`, "unknown content type: video"},
		{"audio with the wrong field types", `{"type":"audio","data":1}`, "cannot unmarshal number"},
		{"embedded resource with no variant", `{"type":"resource","resource":{"uri":"file:///a"}}`, "missing text or blob field"},
		{"embedded resource that is not an object", `{"type":"resource","resource":5}`, "unmarshaling embedded resource"},
		{"embedded resource with malformed _meta", `{"type":"resource","_meta":5,"resource":{"uri":"file:///a","text":"x"}}`, "cannot unmarshal number"},
		{"tool result that is not an object", `{"type":"tool_result","toolUseId":1}`, "cannot unmarshal number"},
		{"tool result with malformed structured content", `{"type":"tool_result","toolUseId":"1","structuredContent":{`, "unexpected end of JSON input"},
		{"tool result with bad nested content", `{"type":"tool_result","toolUseId":"1","content":[{"type":"video"}]}`, "unmarshaling tool result content: unknown content type: video"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := UnmarshalContent([]byte(tt.wire))
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestCompleteParamsUnmarshalRefusesMalformedRefs(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want string
	}{
		{"params not an object", `[]`, "cannot unmarshal array"},
		{"ref not an object", `{"ref": 5}`, "cannot unmarshal number"},
		{"prompt ref with a non-string name", `{"ref":{"type":"ref/prompt","name":1}}`, "cannot unmarshal number"},
		{"resource ref with a non-string uri", `{"ref":{"type":"ref/resource","uri":[]}}`, "cannot unmarshal array"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var params CompleteParams
			require.ErrorContains(t, json.Unmarshal([]byte(tt.wire), &params), tt.want)
		})
	}
}

func TestResourceTemplateURITemplateWire(t *testing.T) {
	template := NewResourceTemplate("file:///{path}", "files")

	encoded, err := json.Marshal(template)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"uriTemplate":"file:///{path}"`)

	var decoded ResourceTemplate
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.NotNil(t, decoded.URITemplate)
	assert.Equal(t, "file:///{path}", decoded.URITemplate.Raw())

	require.ErrorContains(t, json.Unmarshal([]byte(`{"uriTemplate": 5}`), &decoded), "cannot unmarshal number")
	require.ErrorContains(t, json.Unmarshal([]byte(`{"uriTemplate": "file:///{"}`), &decoded), "incomplete expression")
}

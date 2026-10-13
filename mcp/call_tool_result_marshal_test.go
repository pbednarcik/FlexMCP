package mcp

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resultByMarshalJSON is a result as it was written before it could be
// written into the enclosing encoder: MarshalJSON's bytes.
type resultByMarshalJSON struct{ r *CallToolResult }

func (r resultByMarshalJSON) MarshalJSON() ([]byte, error) { return r.r.MarshalJSON() }

// resultEncoders are the ways a result is encoded: the server's own, json's
// other entry points, and v2 encoders with and without v1's options.
var resultEncoders = map[string]func(v any) ([]byte, error){
	"v1 marshal": json.Marshal,
	"v1 encoder": func(v any) ([]byte, error) {
		var out bytes.Buffer
		err := json.NewEncoder(&out).Encode(v)
		return out.Bytes(), err
	},
	"v1 unescaped": func(v any) ([]byte, error) {
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		err := enc.Encode(v)
		return out.Bytes(), err
	},
	"v1 indent":     func(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") },
	"v2":            func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
	"v2 v1 options": func(v any) ([]byte, error) { return jsonv2.Marshal(v, json.DefaultOptionsV1()) },
	"v2 v1 multiline": func(v any) ([]byte, error) {
		return jsonv2.Marshal(v, json.DefaultOptionsV1(), jsontext.Multiline(true))
	},
}

// assertWrittenAsMarshalJSONWrote encodes the result inside a response with
// every encoder, written into the encoder and by MarshalJSON's bytes, and
// requires the same bytes, or the same failure.
func assertWrittenAsMarshalJSONWrote(t *testing.T, result *CallToolResult) {
	t.Helper()
	for name, encode := range resultEncoders {
		want, wantErr := encode(struct {
			ID     int `json:"id"`
			Result any `json:"result"`
		}{1, resultByMarshalJSON{result}})
		got, gotErr := encode(struct {
			ID     int `json:"id"`
			Result any `json:"result"`
		}{1, result})
		require.Equal(t, wantErr != nil, gotErr != nil, "%s: %v, %v", name, wantErr, gotErr)
		assert.Equal(t, string(want), string(got), name)
	}
}

func TestCallToolResultIsWrittenAsMarshalJSONWroteIt(t *testing.T) {
	ttl := int64(60)
	for _, tc := range []struct {
		name   string
		result *CallToolResult
	}{
		{"text", NewToolResultText("ok")},
		{"html and line separators", NewToolResultText("<a & b>  ")},
		{"invalid utf-8", NewToolResultText("a\xffb")},
		{"nil content", &CallToolResult{}},
		{"empty content", &CallToolResult{Content: []Content{}}},
		{"meta and result type", &CallToolResult{
			Meta:       &Meta{ProgressToken: "p", AdditionalFields: map[string]any{"z": 1, "a": "<"}},
			ResultType: ResultTypeComplete,
			Content:    []Content{NewTextContent("x"), NewImageContent("aGk=", "image/png")},
		}},
		{"error", &CallToolResult{Content: []Content{NewTextContent("bad")}, IsError: true}},
		{"raw structured", &CallToolResult{RawStructuredContent: json.RawMessage(` { "b" : 1 , "a" : [ 1.50 , "<" ] } `)}},
		{"structured map", &CallToolResult{StructuredContent: map[string]any{"z": nil, "a": []int(nil), "m": map[string]int{"y": 1, "x": 2}}}},
		{"structured nil map", &CallToolResult{StructuredContent: map[string]any(nil)}},
		{"structured struct", &CallToolResult{StructuredContent: struct {
			A string `json:"a,omitempty"`
			B []byte
		}{"", []byte("hi")}}},
		{"input requests", &CallToolResult{
			RequestState:  "s",
			InputRequests: InputRequests{"q": {Method: MethodElicitationCreate}},
			ResultType:    ResultTypeInputRequired,
		}},
		{"task", &CallToolResult{Task: &ExtensionTask{TaskId: "t", Status: TaskStatusWorking, TTLMs: &ttl}}},
		{"odd strings", &CallToolResult{RequestState: "<\xff> ", ResultType: "a&b"}},
		{"unencodable structured", &CallToolResult{StructuredContent: func() {}}},
	} {
		t.Run(tc.name, func(t *testing.T) { assertWrittenAsMarshalJSONWrote(t, tc.result) })
	}
}

// Any text and any structured content are written as MarshalJSON wrote them.
func FuzzCallToolResultMarshal(f *testing.F) {
	f.Add("ok", `{"a":1}`, false)
	f.Add("<&> ", `[1,"<",{"b":null}]`, true)
	f.Add("\xff", `"\ud800"`, false)
	f.Add("", ``, false)
	f.Fuzz(func(t *testing.T, text, structured string, isError bool) {
		result := &CallToolResult{Content: []Content{NewTextContent(text)}, IsError: isError}
		if json.Valid([]byte(structured)) {
			result.RawStructuredContent = json.RawMessage(structured)
		}
		assertWrittenAsMarshalJSONWrote(t, result)
	})
}

// A result is written into the server's encoder, not encoded on its own first
// and copied in.
func TestCallToolResultMarshalAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	enc := json.NewEncoder(io.Discard)
	response := JSONRPCResponse{JSONRPC: JSONRPC_VERSION, ID: NewRequestId(int64(1)), Result: NewToolResultText("ok")}
	allocs := testing.AllocsPerRun(1000, func() { _ = enc.Encode(response) })
	assert.LessOrEqual(t, allocs, 5.0, "allocations per tools/call response written")
}

// flaggingContent sets its result's error flag while it is encoded.
type flaggingContent struct {
	TextContent
	result *CallToolResult
}

func (c flaggingContent) MarshalJSON() ([]byte, error) {
	c.result.IsError = true
	return json.Marshal(c.TextContent)
}

// A result is written as it was when its encoding began, as MarshalJSON's
// copy of it was: content that changes the result while it is encoded does
// not change what is written.
func TestCallToolResultIsWrittenAsItWasWhenEncodingBegan(t *testing.T) {
	result := &CallToolResult{}
	result.Content = []Content{flaggingContent{NewTextContent("x"), result}}
	want, err := json.Marshal(resultByMarshalJSON{result})
	require.NoError(t, err)
	result.IsError = false
	got, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
	assert.NotContains(t, string(got), "isError")
}

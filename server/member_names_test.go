package server

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// memberCheckOf checks message as the dispatch does, with the method its
// JSON-RPC members decode to.
func memberCheckOf(t *testing.T, message string) memberCheck {
	t.Helper()
	var envelope jsonrpcEnvelope
	require.NoError(t, json.Unmarshal([]byte(message), &envelope))
	return checkMemberNames([]byte(message), envelope.Method)
}

func TestMemberNamesSpelledAsDefinedPass(t *testing.T) {
	for _, tt := range []struct{ name, message string }{
		{"defined member names", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{},"_meta":{"progressToken":1}}}`},
		{"tool arguments keep their own casing", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{"Name":"x","ID":2,"Method":3}}}`},
		{"tool arguments may name a member twice", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":{"a":1,"a":2,"name":3,"name":4}}}`},
		{"unrelated member is not a protocol member", `{"jsonrpc":"2.0","id":1,"method":"ping","extra":true}`},
		{"unrelated member named twice", `{"jsonrpc":"2.0","id":1,"method":"ping","x":1,"x":2}`},
		{"params is null", `{"jsonrpc":"2.0","id":1,"method":"ping","params":null}`},
		{"params is an array", `{"jsonrpc":"2.0","id":1,"method":"ping","params":[{"NAME":1}]}`},
		{"nested values are skipped", `{"jsonrpc":"2.0","id":1,"result":{"Method":[1,{"x":"}"}]},"method":"x"}`},
		{"escaped quotes and backslashes in values", `{"jsonrpc":"2.0","id":"a\"\\","method":"tools/call","params":{"name":"a\\","arguments":{"k":"\\\"}{"}}}`},
		{"escaped defined name", `{"jsonrpc":"2.0","id":1,"\u006dethod":"ping"}`},
		{"whitespace around every token", "{ \"jsonrpc\" : \"2.0\" ,\n\t\"id\" : 1 , \"method\" : \"tools/call\" , \"params\" : { \"name\" : \"t\" } }"},
		{"a notification's params are its handler's own", `{"jsonrpc":"2.0","method":"notifications/custom","params":{"Name":"x","name":"y","name":"z"}}`},
		{"a response", `{"jsonrpc":"2.0","id":1,"result":{"Id":1,"id":2}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := memberCheckOf(t, tt.message)
			assert.NoError(t, check.err())
		})
	}
}

func TestMemberNamesRefused(t *testing.T) {
	for _, tt := range []struct{ name, message, want string }{
		{"capitalized method", `{"jsonrpc":"2.0","id":1,"Method":"tools/call"}`, `member "Method" is not the defined name "method"`},
		{"upper-case method", `{"jsonrpc":"2.0","id":1,"METHOD":"tools/list"}`, `member "METHOD" is not the defined name "method"`},
		{"upper-case jsonrpc", `{"JSONRPC":"2.0","id":1,"method":"tools/list"}`, `member "JSONRPC" is not the defined name "jsonrpc"`},
		{"capitalized id", `{"jsonrpc":"2.0","Id":1,"method":"tools/list"}`, `member "Id" is not the defined name "id"`},
		{"capitalized params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","Params":{"name":"echo"}}`, `member "Params" is not the defined name "params"`},
		{"capitalized result", `{"jsonrpc":"2.0","id":1,"Result":{}}`, `member "Result" is not the defined name "result"`},
		{"U+017F folds onto s", `{"jſonrpc":"2.0","id":1,"method":"ping"}`, `member "jſonrpc" is not the defined name "jsonrpc"`},
		{"U+017F in params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","paramſ":{"name":"echo"}}`, `member "paramſ" is not the defined name "params"`},
		{"U+212A folds onto k", "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tasks/get\",\"params\":{\"tas\u212aId\":\"x\"}}", "member \"params.tas\u212aId\" is not the defined name \"params.taskId\""},
		{"upper-case params name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"echo"}}`, `member "params.NAME" is not the defined name "params.name"`},
		{"capitalized params arguments", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","Arguments":{}}}`, `member "params.Arguments" is not the defined name "params.arguments"`},
		{"upper-case params _meta", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_META":{}}}`, `member "params._META" is not the defined name "params._meta"`},
		{"a cancellation's request id", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"RequestId":1}}`, `member "params.RequestId" is not the defined name "params.requestId"`},
		{"escaped member name", `{"jsonrpc":"2.0","id":1,"\u004dethod":"tools/call"}`, `member "Method" is not the defined name "method"`},
		{"repeated method", `{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"tools/call","params":{"name":"echo"}}`, `duplicate member "method"`},
		{"repeated id", `{"jsonrpc":"2.0","id":1,"method":"tools/list","id":2}`, `duplicate member "id"`},
		{"repeated params name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","name":"echo"}}`, `duplicate member "params.name"`},
		{"after a value holding an escaped quote", `{"jsonrpc":"2.0","id":1,"x":"\"","Method":"ping"}`, `member "Method" is not the defined name "method"`},
		{"after a value holding an escaped backslash", `{"jsonrpc":"2.0","id":1,"x":["\\"],"Method":"ping"}`, `member "Method" is not the defined name "method"`},
		{"after a string holding a brace", `{"jsonrpc":"2.0","id":1,"x":{"y":"}"},"Method":"ping"}`, `member "Method" is not the defined name "method"`},
		{"params before the id", `{"jsonrpc":"2.0","params":{"NAME":"echo"},"method":"tools/call","id":1}`, `member "params.NAME" is not the defined name "params.name"`},
		{"after params", `{"jsonrpc":"2.0","id":1,"params":{"name":"echo","arguments":{"a":"}"}},"Method":"tools/call"}`, `member "Method" is not the defined name "method"`},
		{"the first refusal is the one named", `{"jsonrpc":"2.0","id":1,"METHOD":"tools/call","Params":{"NAME":"x"}}`, `member "METHOD" is not the defined name "method"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := memberCheckOf(t, tt.message)
			assert.EqualError(t, check.err(), tt.want)
		})
	}
}

// Each request the probe for upstream #1166 showed served is refused as an
// invalid request, with the id spelled as defined, and runs nothing.
func TestMemberNamesRefusedRequestRunsNothing(t *testing.T) {
	for _, tt := range []struct {
		name, message string
		id            any
	}{
		{"METHOD", `{"jsonrpc":"2.0","id":1,"METHOD":"tools/list"}`, int64(1)},
		{"JSONRPC", `{"JSONRPC":"2.0","id":1,"method":"tools/list"}`, int64(1)},
		{"Id", `{"jsonrpc":"2.0","Id":1,"method":"tools/list"}`, nil},
		{"Id beside id", `{"jsonrpc":"2.0","Id":1,"id":"a","method":"tools/list"}`, "a"},
		{"Params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","Params":{"name":"echo"}}`, int64(1)},
		{"paramſ", `{"jsonrpc":"2.0","id":1,"method":"tools/call","paramſ":{"name":"echo"}}`, int64(1)},
		{"params.NAME", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"NAME":"echo"}}`, int64(1)},
		{"params.Arguments", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","Arguments":{}}}`, int64(1)},
		{"duplicate method", `{"jsonrpc":"2.0","id":1,"method":"tools/list","method":"tools/call","params":{"name":"echo"}}`, int64(1)},
		{"duplicate id", `{"jsonrpc":"2.0","id":1,"method":"tools/call","id":2,"params":{"name":"echo"}}`, nil},
		{"duplicate params.name", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","name":"echo"}}`, int64(1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var called bool
			srv := NewMCPServer("test", "1.0.0")
			srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				return mcp.NewToolResultText("ran"), nil
			})
			response := srv.HandleMessage(t.Context(), json.RawMessage(tt.message))
			refusal, ok := response.(mcp.JSONRPCError)
			require.True(t, ok, "expected JSONRPCError, got %#v", response)
			assert.Equal(t, mcp.INVALID_REQUEST, refusal.Error.Code)
			assert.Equal(t, tt.id, refusal.ID.Value())
			assert.False(t, called, "the tool ran")
		})
	}
}

// A notification is never answered, a refused one included, and its handler
// does not run; one whose params a handler of its own reads is served.
func TestMemberNamesRefusedNotificationIsDropped(t *testing.T) {
	for _, tt := range []struct {
		name, message string
		runs          bool
	}{
		{"METHOD", `{"jsonrpc":"2.0","METHOD":"notifications/custom"}`, false},
		{"duplicate method", `{"jsonrpc":"2.0","method":"notifications/custom","method":"notifications/custom"}`, false},
		{"Params", `{"jsonrpc":"2.0","method":"notifications/custom","Params":{}}`, false},
		{"its own params", `{"jsonrpc":"2.0","method":"notifications/custom","params":{"Name":1,"name":2}}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var ran bool
			srv := NewMCPServer("test", "1.0.0")
			srv.AddNotificationHandler("notifications/custom", func(context.Context, mcp.JSONRPCNotification) { ran = true })
			assert.Nil(t, srv.HandleMessage(t.Context(), json.RawMessage(tt.message)))
			assert.Equal(t, tt.runs, ran, "the handler ran")
		})
	}
}

// A response is never answered, one with a member named twice included.
func TestMemberNamesRefusedResponseIsDropped(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0")
	assert.Nil(t, srv.HandleMessage(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":5,"result":{},"result":{}}`)))
}

// Tool arguments are the caller's own: any spelling and a key named twice
// reach the tool as encoding/json reads them.
func TestMemberNamesToolArgumentsReachTheTool(t *testing.T) {
	var arguments map[string]any
	srv := NewMCPServer("test", "1.0.0")
	srv.AddTool(mcp.NewTool("echo"), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments = request.GetArguments()
		return mcp.NewToolResultText("ran"), nil
	})
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"Name":"x","a":1,"a":2}}}`
	response := srv.HandleMessage(t.Context(), json.RawMessage(message))
	_, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSONRPCResponse, got %#v", response)
	assert.Equal(t, map[string]any{"Name": "x", "a": float64(2)}, arguments)
}

// decodedRequests are the request types dispatchRequest decodes, by name.
var decodedRequests = map[string]any{
	"InitializeRequest": mcp.InitializeRequest{}, "PingRequest": mcp.PingRequest{},
	"DiscoverRequest": mcp.DiscoverRequest{}, "SubscriptionsListenRequest": mcp.SubscriptionsListenRequest{},
	"SetLevelRequest": mcp.SetLevelRequest{}, "ListResourcesRequest": mcp.ListResourcesRequest{},
	"ListResourceTemplatesRequest": mcp.ListResourceTemplatesRequest{}, "ReadResourceRequest": mcp.ReadResourceRequest{},
	"ListPromptsRequest": mcp.ListPromptsRequest{}, "GetPromptRequest": mcp.GetPromptRequest{},
	"ListToolsRequest": mcp.ListToolsRequest{}, "CallToolRequest": mcp.CallToolRequest{},
	"ExtensionTaskRequest": mcp.ExtensionTaskRequest{}, "GetTaskRequest": mcp.GetTaskRequest{},
	"ListTasksRequest": mcp.ListTasksRequest{}, "TaskResultRequest": mcp.TaskResultRequest{},
	"CancelTaskRequest": mcp.CancelTaskRequest{}, "UpdateExtensionTaskRequest": mcp.UpdateExtensionTaskRequest{},
	"CompleteRequest": mcp.CompleteRequest{},
}

// The params members checked are exactly those the server decodes into
// fields: of every request type dispatchRequest decodes, and of the read
// before dispatch, which holds the one a cancellation names.
func TestMemberNamesParamsAreTheDecodedOnes(t *testing.T) {
	source, err := os.ReadFile("request_handler.go")
	require.NoError(t, err)
	var dispatched []string
	for _, m := range regexp.MustCompile(`var request mcp\.(\w+)`).FindAllSubmatch(source, -1) {
		dispatched = append(dispatched, string(m[1]))
	}
	slices.Sort(dispatched)
	dispatched = slices.Compact(dispatched)
	listed := make([]string, 0, len(decodedRequests))
	for name := range decodedRequests {
		listed = append(listed, name)
	}
	slices.Sort(listed)
	require.Equal(t, dispatched, listed, "the request types dispatchRequest decodes")

	decoded := jsonNames(reflect.TypeFor[paramsPeek]())
	for name, request := range decodedRequests {
		params, ok := reflect.TypeOf(request).FieldByName("Params")
		require.True(t, ok, "%s has no Params", name)
		decoded = append(decoded, jsonNames(params.Type)...)
	}
	slices.Sort(decoded)
	decoded = slices.Compact(decoded)
	checked := make([]string, 0, len(paramsMembers))
	for _, name := range paramsMembers {
		checked = append(checked, string(name))
	}
	slices.Sort(checked)
	assert.Equal(t, decoded, checked)
}

// jsonNames is the JSON member names a struct type decodes, embedded
// structs' included.
func jsonNames(typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}
	var names []string
	for f := range typ.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case f.Anonymous && name == "":
			names = append(names, jsonNames(f.Type)...)
		case f.IsExported():
			if name == "" {
				name = f.Name
			}
			names = append(names, name)
		}
	}
	return names
}

// typicalToolCall is a 2026-07-28 tools/call as a client sends one.
const typicalToolCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"region":"us-east-1"},` +
	`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},` +
	`"io.modelcontextprotocol/clientInfo":{"name":"bench","version":"1.0.0"},"progressToken":"p-1"}}}`

// The check runs on every message: one that passes allocates nothing.
func TestMemberNamesCheckAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	message := []byte(typicalToolCall)
	allocs := testing.AllocsPerRun(100, func() {
		if checkMemberNames(message, mcp.MethodToolsCall).err() != nil {
			t.Fatal("refused")
		}
	})
	assert.Zero(t, allocs)
}

func BenchmarkCheckMemberNames(b *testing.B) {
	message := []byte(typicalToolCall)
	b.SetBytes(int64(len(message)))
	b.ReportAllocs()
	for b.Loop() {
		checkMemberNames(message, mcp.MethodToolsCall)
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendIn sends method with params to srv through HandleMessage in era and
// returns the JSON-RPC answer as the client reads it.
func sendIn(t *testing.T, srv *MCPServer, method mcp.MCPMethod, params map[string]any, era callEra) map[string]any {
	t.Helper()
	ctx := t.Context()
	if era != legacyCall {
		meta := modernMeta()
		if era == declaredCall {
			meta[mcp.MetaKeyClientCapabilities] = declaringTasks()
		}
		params["_meta"] = meta
		header := http.Header{
			"Mcp-Protocol-Version": {mcp.ProtocolVersion20260728},
			"Mcp-Method":           {string(method)},
		}
		if name, ok := params["name"].(string); ok {
			encoded, ok := mcp.EncodeHeaderValue(name)
			require.True(t, ok)
			header["Mcp-Name"] = []string{encoded}
		}
		ctx = context.WithValue(ctx, requestHeader, header)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	out, err := json.Marshal(srv.HandleMessage(ctx, body))
	require.NoError(t, err)
	var message map[string]any
	require.NoError(t, json.Unmarshal(out, &message))
	return message
}

// taskKinds registers one tool of each kind a 2025-11-25 task can involve, on
// a server that serves tasks. The handlers of the regular ones count their
// runs in ran.
func taskKinds(t *testing.T, ran *int, opts ...ServerOption) *MCPServer {
	t.Helper()
	srv := NewMCPServer("legacy-era", "1.0.0", append([]ServerOption{
		WithToolCapabilities(true),
		WithTaskCapabilities(true, true, true),
	}, opts...)...)
	handler := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		*ran++
		return mcp.NewToolResultText("sync"), nil
	}
	srv.AddTool(mcp.NewTool("plain"), handler)
	srv.AddTool(mcp.NewTool("optional", mcp.WithTaskSupport(mcp.TaskSupportOptional)), handler)
	srv.AddTool(mcp.NewTool("forbidden", mcp.WithTaskSupport(mcp.TaskSupportForbidden)), handler)
	srv.AddTool(mcp.NewTool("required", mcp.WithTaskSupport(mcp.TaskSupportRequired)), handler)
	srv.AddTaskTool(mcp.NewTool("only", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			return &mcp.CreateTaskResult{}, nil
		})
	return srv
}

// listedTools returns the tools of a tools/list answer, as the client reads
// them.
func listedTools(t *testing.T, srv *MCPServer, era callEra) []map[string]any {
	t.Helper()
	message := sendIn(t, srv, mcp.MethodToolsList, map[string]any{}, era)
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	raw, ok := result["tools"].([]any)
	require.True(t, ok, "expected tools, got %v", result)
	tools := make([]map[string]any, len(raw))
	for i, entry := range raw {
		tools[i], ok = entry.(map[string]any)
		require.True(t, ok)
	}
	return tools
}

func toolNames(tools []map[string]any) []string {
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i], _ = tool["name"].(string)
	}
	slices.Sort(names)
	return names
}

func taskCount(srv *MCPServer) int {
	srv.tasksMu.RLock()
	defer srv.tasksMu.RUnlock()
	return len(srv.tasks)
}

func TestModernToolCallIgnoresParamsTask(t *testing.T) {
	for _, era := range []callEra{undeclaredCall, declaredCall} {
		t.Run(map[callEra]string{undeclaredCall: "undeclared", declaredCall: "declared"}[era], func(t *testing.T) {
			var ran int
			var seen []*mcp.TaskParams
			srv := taskKinds(t, &ran, WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
				return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					seen = append(seen, request.Params.Task)
					return next(ctx, request)
				}
			}))

			message := sendIn(t, srv, mcp.MethodToolsCall,
				map[string]any{"name": "optional", "task": map[string]any{"ttl": 1000}}, era)
			result, ok := message["result"].(map[string]any)
			require.True(t, ok, "expected a result, got %v", message)
			assert.Equal(t, []any{map[string]any{"type": "text", "text": "sync"}}, result["content"])
			assert.NotContains(t, result, "task", "the call became a task")
			assert.Equal(t, 1, ran, "the tool did not run synchronously")
			assert.Equal(t, []*mcp.TaskParams{nil}, seen, "the middleware saw params.task")
			assert.Zero(t, taskCount(srv), "a 2025-11-25 task was stored")
		})
	}
}

// A call that could become a task of this extension still can: params.task is
// cleared, the declaration is not.
func TestModernToolCallWithParamsTaskCanStillHandOver(t *testing.T) {
	srv := NewMCPServer("legacy-era", "1.0.0",
		WithToolCapabilities(true),
		WithTaskCapabilities(true, true, true),
		WithTaskHandler(stubTaskHandler{}))
	srv.AddTool(mcp.NewTool("optional", mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return NewTaskResult(ctx, "job-1")
		})
	message := sendIn(t, srv, mcp.MethodToolsCall,
		map[string]any{"name": "optional", "task": map[string]any{"ttl": 1000}}, declaredCall)
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	assert.Equal(t, "task", result["resultType"])
	assert.Zero(t, taskCount(srv))
}

func TestModernToolsListHidesTaskOnlyTools(t *testing.T) {
	var ran int
	srv := taskKinds(t, &ran)

	for _, era := range []callEra{undeclaredCall, declaredCall} {
		tools := listedTools(t, srv, era)
		assert.Equal(t, []string{"forbidden", "optional", "plain"}, toolNames(tools), "era %d", era)
		for _, tool := range tools {
			assert.NotContains(t, tool, "execution", "era %d: %v", era, tool["name"])
		}
	}

	// The registered tools are untouched by the modern listing.
	registered := srv.ListTools()
	for _, name := range []string{"optional", "forbidden", "required"} {
		require.NotNil(t, registered[name].Tool.Execution, name)
	}

	tools := listedTools(t, srv, legacyCall)
	assert.Equal(t, []string{"forbidden", "only", "optional", "plain", "required"}, toolNames(tools))
	for _, tool := range tools {
		if tool["name"] == "plain" {
			assert.NotContains(t, tool, "execution")
			continue
		}
		assert.Contains(t, tool, "execution", "%v", tool["name"])
	}
}

func TestModernCallToATaskOnlyToolIsNotFound(t *testing.T) {
	for _, name := range []string{"only", "required"} {
		for _, withTask := range []bool{false, true} {
			var ran int
			srv := taskKinds(t, &ran)
			params := map[string]any{"name": name}
			if withTask {
				params["task"] = map[string]any{"ttl": 1000}
			}
			for _, era := range []callEra{undeclaredCall, declaredCall} {
				message := sendIn(t, srv, mcp.MethodToolsCall, params, era)
				assert.Equal(t, mcp.METHOD_NOT_FOUND, errorCode(t, message), "%s, params.task %v, era %d", name, withTask, era)
			}
			assert.Zero(t, ran)
			assert.Zero(t, taskCount(srv))
		}
	}
}

func TestLegacyTaskAugmentationUnchanged(t *testing.T) {
	var ran int
	srv := taskKinds(t, &ran)
	message := sendIn(t, srv, mcp.MethodToolsCall,
		map[string]any{"name": "optional", "task": map[string]any{"ttl": 1000}}, legacyCall)
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	task, ok := result["task"].(map[string]any)
	require.True(t, ok, "expected a CreateTaskResult, got %v", result)
	assert.NotEmpty(t, task["taskId"])
	assert.Equal(t, 1, taskCount(srv))
}

// listPage returns the names on one page of tools/list in era after cursor,
// and the cursor of the next page, empty on the last. A page that fills the
// limit carries a cursor, so the last one may be empty.
func listPage(t *testing.T, srv *MCPServer, era callEra, cursor string) ([]string, string) {
	t.Helper()
	params := map[string]any{}
	if cursor != "" {
		params["cursor"] = cursor
	}
	message := sendIn(t, srv, mcp.MethodToolsList, params, era)
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	var names []string
	for _, entry := range result["tools"].([]any) {
		names = append(names, entry.(map[string]any)["name"].(string))
	}
	next, _ := result["nextCursor"].(string)
	return names, next
}

// listPages walks tools/list in era to its end, and returns every page and
// the cursor each one issued. A page that is not the last is never empty.
func listPages(t *testing.T, srv *MCPServer, era callEra) ([][]string, []string) {
	t.Helper()
	var pages [][]string
	var cursors []string
	cursor := ""
	for {
		names, next := listPage(t, srv, era, cursor)
		pages = append(pages, names)
		if next == "" {
			break
		}
		cursors = append(cursors, next)
		cursor = next
	}
	// The page that fills the limit last issues a cursor to an empty one.
	if last := len(pages) - 1; len(pages[last]) == 0 {
		pages = pages[:last]
	}
	return pages, cursors
}

// The pages of a modern list hold listed tools only, none empty, and a cursor
// an earlier era issued for a tool the modern list hides still resumes after it.
func TestModernToolsListPagesSkipHiddenTools(t *testing.T) {
	var ran int
	srv := taskKinds(t, &ran, WithPaginationLimit(1))
	srv.AddTool(mcp.NewTool("zeta"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("sync"), nil
	})

	legacyPages, legacyCursors := listPages(t, srv, legacyCall)
	require.Equal(t, [][]string{{"forbidden"}, {"only"}, {"optional"}, {"plain"}, {"required"}, {"zeta"}}, legacyPages)

	modernPages, _ := listPages(t, srv, undeclaredCall)
	assert.Equal(t, [][]string{{"forbidden"}, {"optional"}, {"plain"}, {"zeta"}}, modernPages)

	// legacyCursors[i] follows legacy page i: after only, and after required.
	names, _ := listPage(t, srv, undeclaredCall, legacyCursors[1])
	assert.Equal(t, []string{"optional"}, names, "a cursor naming a hidden task-only tool")
	names, _ = listPage(t, srv, undeclaredCall, legacyCursors[4])
	assert.Equal(t, []string{"zeta"}, names, "a cursor naming a hidden required tool")
}

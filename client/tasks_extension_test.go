package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const extensionTaskID = "dGFzay1hYmM"

// withTasksExtension configures a client to declare the Tasks extension, as
// the extension's methods need.
func withTasksExtension() ClientOption {
	return WithClientCapabilities(mcp.ClientCapabilities{Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}}})
}

// stateJSON renders a tasks/get answer for status; extra is the members after
// the task fields, with their leading comma.
func stateJSON(status string, pollIntervalMs *int64, extra string) string {
	poll := ""
	if pollIntervalMs != nil {
		poll = fmt.Sprintf(`,"pollIntervalMs":%d`, *pollIntervalMs)
	}
	return fmt.Sprintf(`{"resultType":"complete","taskId":%q,"status":%q,"createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttlMs":null%s%s}`,
		extensionTaskID, status, poll, extra)
}

// safetyNet is a context with a deadline, so a wait that fails to return
// fails its test instead of hanging it; a passing wait never reaches it. Under
// synctest the deadline runs on the fake clock.
func safetyNet(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func errorResponse(request transport.JSONRPCRequest, code int, message string, data any) *transport.JSONRPCResponse {
	return &transport.JSONRPCResponse{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      request.ID,
		Error:   &mcp.JSONRPCErrorDetails{Code: code, Message: message, Data: data},
	}
}

// taskCall is a request a scripted server saw.
type taskCall struct {
	method string
	taskID string
	header http.Header
	params json.RawMessage
}

// taskRecorder answers the three task methods and records what it was asked.
type taskRecorder struct {
	mu    sync.Mutex
	calls []taskCall
	// answer renders the result of the nth tasks/get, counting from zero.
	answer func(n int) string
}

func (r *taskRecorder) respond(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	encoded, err := json.Marshal(request.Params)
	if err != nil {
		return nil, err
	}
	var params struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(encoded, &params)
	r.mu.Lock()
	defer r.mu.Unlock()
	gets := 0
	for _, c := range r.calls {
		if c.method == string(mcp.MethodTasksGet) {
			gets++
		}
	}
	r.calls = append(r.calls, taskCall{method: request.Method, taskID: params.TaskID, header: request.Header.Clone(), params: encoded})
	if request.Method == string(mcp.MethodTasksGet) {
		return resultResponse(request, r.answer(gets)), nil
	}
	return resultResponse(request, `{"resultType":"complete"}`), nil
}

func (r *taskRecorder) seen() []taskCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]taskCall(nil), r.calls...)
}

// Each Tasks extension method sends its method name, the task id in
// params.taskId, and the Mcp-Method and Mcp-Name headers (Mcp-Name is the
// task id), and declares the extension in the capabilities of its request.
func TestExtensionTaskMethodsSendTheirMethodTaskIDAndHeaders(t *testing.T) {
	responses := mcp.InputResponses{"k": mcp.NewRootsInputResponse(mcp.ListRootsResult{})}
	tests := []struct {
		name   string
		method mcp.MCPMethod
		call   func(c *Client) error
	}{
		{"tasks/get", mcp.MethodTasksGet, func(c *Client) error {
			state, err := c.GetExtensionTask(t.Context(), extensionTaskID)
			if err == nil {
				assert.Equal(t, extensionTaskID, state.TaskId)
				assert.Equal(t, mcp.TaskStatusWorking, state.Status)
			}
			return err
		}},
		{"tasks/cancel", mcp.MethodTasksCancel, func(c *Client) error {
			return c.CancelExtensionTask(t.Context(), extensionTaskID)
		}},
		{"tasks/update", mcp.MethodTasksUpdate, func(c *Client) error {
			return c.UpdateExtensionTask(t.Context(), extensionTaskID, responses)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &taskRecorder{answer: func(int) string { return stateJSON("working", nil, "") }}
			var declared json.RawMessage
			tr := &scriptedTransport{respond: modernScript(func(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				declared = requestMeta(t, request)[mcp.MetaKeyClientCapabilities]
				return rec.respond(ctx, request)
			})}
			c := connectScripted(t, tr, withTasksExtension())

			require.NoError(t, tt.call(c))

			calls := rec.seen()
			require.Len(t, calls, 1)
			assert.Equal(t, string(tt.method), calls[0].method)
			assert.Equal(t, extensionTaskID, calls[0].taskID)
			assert.Equal(t, string(tt.method), calls[0].header.Get(mcp.HeaderMethod))
			assert.Equal(t, extensionTaskID, calls[0].header.Get(mcp.HeaderName))
			assert.Equal(t, mcp.ProtocolVersion20260728, calls[0].header.Get(mcp.HeaderProtocolVersion))
			assert.JSONEq(t, `{"extensions":{"io.modelcontextprotocol/tasks":{}}}`, string(declared), "the request declares the extension")
			if tt.method == mcp.MethodTasksUpdate {
				var sent struct {
					InputResponses map[string]json.RawMessage `json:"inputResponses"`
				}
				require.NoError(t, json.Unmarshal(calls[0].params, &sent))
				assert.Contains(t, sent.InputResponses, "k")
			}
		})
	}
}

// The same over a real Streamable HTTP transport: the headers are the ones
// the HTTP request carried.
func TestExtensionTaskMethodsWriteMcpNameOverStreamableHTTP(t *testing.T) {
	var mu sync.Mutex
	headers := map[string]http.Header{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				TaskID string `json:"taskId"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case string(mcp.MethodServerDiscover):
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, discoverResult)
		case string(mcp.MethodTasksGet):
			mu.Lock()
			headers[request.Method] = r.Header.Clone()
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, stateJSON("working", nil, ""))
		case string(mcp.MethodTasksCancel), string(mcp.MethodTasksUpdate):
			mu.Lock()
			headers[request.Method] = r.Header.Clone()
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete"}}`, request.ID)
		default:
			t.Errorf("unexpected method %q", request.Method)
		}
	}))
	defer server.Close()
	trans, err := transport.NewStreamableHTTP(server.URL)
	require.NoError(t, err)
	c := NewClient(trans, withTasksExtension())
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	require.True(t, c.isModern())

	_, err = c.GetExtensionTask(t.Context(), extensionTaskID)
	require.NoError(t, err)
	require.NoError(t, c.CancelExtensionTask(t.Context(), extensionTaskID))
	require.NoError(t, c.UpdateExtensionTask(t.Context(), extensionTaskID, nil))

	mu.Lock()
	defer mu.Unlock()
	for _, method := range []mcp.MCPMethod{mcp.MethodTasksGet, mcp.MethodTasksCancel, mcp.MethodTasksUpdate} {
		h, ok := headers[string(method)]
		require.True(t, ok, "%s reached the server", method)
		assert.Equal(t, string(method), h.Get(mcp.HeaderMethod), method)
		assert.Equal(t, extensionTaskID, h.Get(mcp.HeaderName), method)
	}
}

// A client declares the Tasks extension only when its caller configured it;
// the extension's methods refuse otherwise, before anything is sent, and on a
// connection that is not 2026-07-28.
func TestExtensionTaskMethodsNeedTheDeclaredExtension(t *testing.T) {
	calls := map[string]func(c *Client) error{
		"GetExtensionTask": func(c *Client) error {
			_, err := c.GetExtensionTask(t.Context(), extensionTaskID)
			return err
		},
		"CancelExtensionTask": func(c *Client) error { return c.CancelExtensionTask(t.Context(), extensionTaskID) },
		"UpdateExtensionTask": func(c *Client) error { return c.UpdateExtensionTask(t.Context(), extensionTaskID, nil) },
		"WaitForExtensionTask": func(c *Client) error {
			_, err := c.WaitForExtensionTask(t.Context(), extensionTaskID, time.Second)
			return err
		},
	}
	setups := map[string]struct {
		options []ClientOption
		legacy  bool
	}{
		"not configured":      {},
		"handshake era":       {options: []ClientOption{withTasksExtension(), WithLegacyProtocolOnly()}, legacy: true},
		"only the 2025 tasks": {options: []ClientOption{WithClientCapabilities(mcp.ClientCapabilities{Tasks: &mcp.TasksCapability{}})}},
	}
	for callName, call := range calls {
		for setupName, setup := range setups {
			t.Run(callName+"/"+setupName, func(t *testing.T) {
				tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
					if setup.legacy && request.Method == string(mcp.MethodInitialize) {
						return resultResponse(request, legacyInitializeResult), nil
					}
					return resultResponse(request, stateJSON("working", nil, "")), nil
				})}
				c := connectScripted(t, tr, setup.options...)
				before := len(tr.sentMethods())

				err := call(c)

				require.ErrorIs(t, err, ErrExtensionTasksNotDeclared)
				assert.ErrorContains(t, err, mcp.ExtensionTasks)
				assert.Len(t, tr.sentMethods(), before, "nothing was sent")
			})
		}
	}
}

// tasks/get returns the task, with the member its status carries, and the
// result type is judged.
func TestGetExtensionTaskReturnsTheTaskWithItsStatusMember(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		check  func(t *testing.T, state *mcp.ExtensionTaskStateResult)
	}{
		{"completed", stateJSON("completed", nil, `,"result":{"content":[{"type":"text","text":"done"}]}`), func(t *testing.T, s *mcp.ExtensionTaskStateResult) {
			assert.Equal(t, mcp.TaskStatusCompleted, s.Status)
			require.NotNil(t, s.TaskResult)
			assert.Equal(t, "done", s.TaskResult.Content[0].(mcp.TextContent).Text)
		}},
		{"failed", stateJSON("failed", nil, `,"error":{"code":-32603,"message":"boom"}`), func(t *testing.T, s *mcp.ExtensionTaskStateResult) {
			assert.Equal(t, mcp.TaskStatusFailed, s.Status)
			require.NotNil(t, s.Error)
			assert.Equal(t, "boom", s.Error.Message)
		}},
		{"input_required", stateJSON("input_required", nil, `,"inputRequests":{"k":{"method":"roots/list"}}`), func(t *testing.T, s *mcp.ExtensionTaskStateResult) {
			assert.Equal(t, mcp.TaskStatusInputRequired, s.Status)
			assert.Contains(t, s.InputRequests, "k")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return resultResponse(request, tt.answer), nil
			})}
			c := connectScripted(t, tr, withTasksExtension())

			state, err := c.GetExtensionTask(t.Context(), extensionTaskID)

			require.NoError(t, err)
			tt.check(t, state)
		})
	}

	t.Run("an unaccepted resultType", func(t *testing.T) {
		tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			return resultResponse(request, `{"resultType":"task","taskId":"t","status":"working"}`), nil
		})}
		c := connectScripted(t, tr, withTasksExtension())

		_, err := c.GetExtensionTask(t.Context(), extensionTaskID)

		assert.ErrorIs(t, err, errUnacceptedResultType)
	})
}

// A tasks/get answer that breaks the extension's rules is refused, by
// GetExtensionTask and by WaitForExtensionTask, with an error naming what is
// wrong and neither task id: another task than the one asked for, a
// completed task without its result or with one that is a task or not
// complete, a failed task without its error.
func TestGetExtensionTaskRefusesAnInvalidState(t *testing.T) {
	const task = `"createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttlMs":null`
	nestedTask := `,"result":{"resultType":"task","taskId":"nested","status":"working",` + task + `}`
	tests := map[string]string{
		"another task":                       `{"resultType":"complete","taskId":"different","status":"working",` + task + `}`,
		"completed without its result":       stateJSON("completed", nil, ""),
		"completed with a task":              stateJSON("completed", nil, nestedTask),
		"completed with input_required":      stateJSON("completed", nil, `,"result":{"resultType":"input_required","content":[],"inputRequests":{"k":{"method":"roots/list"}}}`),
		"completed with another result type": stateJSON("completed", nil, `,"result":{"resultType":"partial","content":[]}`),
		"failed without its error":           stateJSON("failed", nil, ""),
	}
	for name, answer := range tests {
		for _, wait := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wait=%v", name, wait), func(t *testing.T) {
				tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
					return resultResponse(request, answer), nil
				})}
				c := connectScripted(t, tr, withTasksExtension())

				var state *mcp.ExtensionTaskStateResult
				var err error
				if wait {
					// A wait that took the state for a working task would poll on.
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
					defer cancel()
					state, err = c.WaitForExtensionTask(ctx, extensionTaskID, time.Millisecond)
				} else {
					state, err = c.GetExtensionTask(t.Context(), extensionTaskID)
				}

				require.ErrorIs(t, err, ErrInvalidTaskState)
				assert.Nil(t, state)
				assert.NotContains(t, err.Error(), extensionTaskID)
				assert.NotContains(t, err.Error(), "different")
				assert.NotContains(t, err.Error(), "nested")
			})
		}
	}
}

// An answer without resultType is complete (a client MUST read a missing one
// so): GetExtensionTask and WaitForExtensionTask take a tasks/get answer
// without one, and CancelExtensionTask and UpdateExtensionTask an
// acknowledgement without one.
func TestExtensionTaskMethodsTakeAnAnswerWithoutResultType(t *testing.T) {
	state := `{"taskId":"` + extensionTaskID + `","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:01Z","ttlMs":null,"result":{"content":[]}}`
	calls := map[string]func(c *Client) error{
		"get": func(c *Client) error {
			got, err := c.GetExtensionTask(t.Context(), extensionTaskID)
			if err == nil {
				assert.Equal(t, mcp.TaskStatusCompleted, got.Status)
			}
			return err
		},
		"wait": func(c *Client) error {
			got, err := c.WaitForExtensionTask(t.Context(), extensionTaskID, time.Millisecond)
			if err == nil {
				assert.Equal(t, mcp.TaskStatusCompleted, got.Status)
			}
			return err
		},
		"cancel": func(c *Client) error { return c.CancelExtensionTask(t.Context(), extensionTaskID) },
		"update": func(c *Client) error { return c.UpdateExtensionTask(t.Context(), extensionTaskID, nil) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				if request.Method == string(mcp.MethodTasksGet) {
					return resultResponse(request, state), nil
				}
				return resultResponse(request, `{}`), nil
			})}
			c := connectScripted(t, tr, withTasksExtension())

			require.NoError(t, call(c))
		})
	}
}

// The server's -32602, for an id it does not know or one that is gone, reaches
// the caller as an mcp.TaskRefusedError with the server's reason, which is
// still an mcp.ErrInvalidParams. A -32602 without a reason stays an ordinary
// one.
func TestExtensionTaskMethodsSurfaceAnUnknownOrGoneTask(t *testing.T) {
	calls := map[string]func(c *Client) error{
		"get": func(c *Client) error {
			_, err := c.GetExtensionTask(t.Context(), extensionTaskID)
			return err
		},
		"cancel": func(c *Client) error { return c.CancelExtensionTask(t.Context(), extensionTaskID) },
		"update": func(c *Client) error { return c.UpdateExtensionTask(t.Context(), extensionTaskID, nil) },
		"wait": func(c *Client) error {
			_, err := c.WaitForExtensionTask(t.Context(), extensionTaskID, time.Second)
			return err
		},
	}
	for name, call := range calls {
		for _, reason := range []string{"unknown", "gone"} {
			t.Run(name+"/"+reason, func(t *testing.T) {
				tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
					return errorResponse(request, mcp.INVALID_PARAMS, reason+": the server's words", map[string]any{"reason": reason}), nil
				})}
				c := connectScripted(t, tr, withTasksExtension())

				err := call(c)

				require.ErrorIs(t, err, mcp.ErrInvalidParams)
				var refused mcp.TaskRefusedError
				require.ErrorAs(t, err, &refused)
				assert.Equal(t, reason, refused.Reason)
				assert.Equal(t, reason+": the server's words", refused.Message)
				assert.Equal(t, reason == "gone", mcp.IsTaskGone(err))
			})
		}
	}

	t.Run("a -32602 without a task reason", func(t *testing.T) {
		for name, data := range map[string]any{"no data": nil, "another reason": map[string]any{"reason": "rude"}} {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return errorResponse(request, mcp.INVALID_PARAMS, "bad params", data), nil
			})}
			c := connectScripted(t, tr, withTasksExtension())

			_, err := c.GetExtensionTask(t.Context(), extensionTaskID)

			require.ErrorIs(t, err, mcp.ErrInvalidParams, name)
			var refused mcp.TaskRefusedError
			assert.False(t, errors.As(err, &refused), name)
			assert.False(t, mcp.IsTaskGone(err), name)
		}
	})
}

// The server's -32021 for a request that did not declare the extension is its
// typed error.
func TestExtensionTaskMethodsSurfaceMissingCapability(t *testing.T) {
	tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return errorResponse(request, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY, "missing", map[string]any{
			"requiredCapabilities": map[string]any{"extensions": map[string]any{mcp.ExtensionTasks: map[string]any{}}},
		}), nil
	})}
	c := connectScripted(t, tr, withTasksExtension())

	_, err := c.GetExtensionTask(t.Context(), extensionTaskID)

	assert.True(t, mcp.IsMissingRequiredClientCapability(err), "%v", err)
}

// WaitForExtensionTask waits the server's pollIntervalMs between polls, never
// less than 250 ms and with no ceiling; a missing interval is the caller's
// fallback. The clock is synctest's, so the waits are exact.
func TestWaitForExtensionTaskWaitsTheServersIntervalWithAFloor(t *testing.T) {
	tests := []struct {
		name     string
		poll     *int64
		fallback time.Duration
		want     time.Duration
	}{
		{"a 1 ms interval is raised to the 250 ms floor", new(int64(1)), time.Second, 250 * time.Millisecond},
		{"an interval over the floor is kept", new(int64(400)), time.Second, 400 * time.Millisecond},
		{"a 60 s interval is respected, with no ceiling", new(int64(60_000)), time.Second, 60 * time.Second},
		{"an hour is respected too", new(int64(3_600_000)), time.Second, time.Hour},
		{"an absent interval is the fallback", nil, 2 * time.Second, 2 * time.Second},
		{"a zero interval is the fallback", new(int64(0)), 3 * time.Second, 3 * time.Second},
		{"a short fallback is the caller's own", nil, 10 * time.Millisecond, 10 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var polledAt []time.Time
				rec := &taskRecorder{answer: func(n int) string {
					mu.Lock()
					polledAt = append(polledAt, time.Now())
					mu.Unlock()
					if n == 0 {
						return stateJSON("working", tt.poll, "")
					}
					return stateJSON("completed", nil, `,"result":{"content":[]}`)
				}}
				tr := &scriptedTransport{respond: modernScript(rec.respond)}
				c := connectScripted(t, tr, withTasksExtension())

				state, err := c.WaitForExtensionTask(t.Context(), extensionTaskID, tt.fallback)

				require.NoError(t, err)
				assert.Equal(t, mcp.TaskStatusCompleted, state.Status)
				mu.Lock()
				defer mu.Unlock()
				require.Len(t, polledAt, 2)
				assert.Equal(t, tt.want, polledAt[1].Sub(polledAt[0]), "the wait between the two polls")
			})
		})
	}
}

// terminalMembers are the members a tasks/get answer carries for a status
// that needs one.
var terminalMembers = map[string]string{
	"completed": `,"result":{"content":[]}`,
	"failed":    `,"error":{"code":-32603,"message":"boom"}`,
}

// A terminal status ends the wait, and so does input_required, which only
// the caller can move on: the state is returned and nothing is polled again.
func TestWaitForExtensionTaskReturnsOnTerminalAndInputRequired(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled", "input_required"} {
		t.Run(status, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &taskRecorder{answer: func(n int) string {
					if n == 0 {
						return stateJSON("working", new(int64(1)), "")
					}
					return stateJSON(status, nil, terminalMembers[status])
				}}
				tr := &scriptedTransport{respond: modernScript(rec.respond)}
				c := connectScripted(t, tr, withTasksExtension())
				ctx := safetyNet(t)

				state, err := c.WaitForExtensionTask(ctx, extensionTaskID, time.Millisecond)

				require.NoError(t, err)
				assert.Equal(t, mcp.TaskStatus(status), state.Status)
				assert.Len(t, rec.seen(), 2, "one working poll and the returning one")
			})
		})
	}

	t.Run("an already returning task is polled once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rec := &taskRecorder{answer: func(int) string { return stateJSON("input_required", new(int64(60_000)), "") }}
			tr := &scriptedTransport{respond: modernScript(rec.respond)}
			c := connectScripted(t, tr, withTasksExtension())
			ctx := safetyNet(t)

			state, err := c.WaitForExtensionTask(ctx, extensionTaskID, time.Second)

			require.NoError(t, err)
			assert.Equal(t, mcp.TaskStatusInputRequired, state.Status)
			assert.Len(t, rec.seen(), 1)
		})
	})
}

// A status outside the five the extension defines is a server's mistake, and
// no later poll will make it terminal: the wait ends with the state and an
// error naming the status, but not the task id. A long status is cut.
func TestWaitForExtensionTaskStopsOnAnUnknownStatus(t *testing.T) {
	long := strings.Repeat("x", 500)
	for name, status := range map[string]string{"empty": "", "unheard of": "paused", "long": long} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := &taskRecorder{answer: func(int) string { return stateJSON(status, nil, "") }}
				tr := &scriptedTransport{respond: modernScript(rec.respond)}
				c := connectScripted(t, tr, withTasksExtension())
				ctx := safetyNet(t)

				state, err := c.WaitForExtensionTask(ctx, extensionTaskID, time.Second)

				require.ErrorIs(t, err, ErrUnknownTaskStatus)
				require.NotNil(t, state, "the state is returned with the error")
				assert.Equal(t, mcp.TaskStatus(status), state.Status)
				assert.NotContains(t, err.Error(), extensionTaskID)
				assert.Contains(t, err.Error(), fmt.Sprintf("%.64q", status))
				assert.Less(t, len(err.Error()), 300)
				assert.Len(t, rec.seen(), 1, "polled once")
			})
		})
	}
}

// UpdateExtensionTask with no responses sends an empty object for the
// required inputResponses member, not null.
func TestUpdateExtensionTaskSendsEmptyInputResponsesForNil(t *testing.T) {
	rec := &taskRecorder{answer: func(int) string { return stateJSON("working", nil, "") }}
	tr := &scriptedTransport{respond: modernScript(rec.respond)}
	c := connectScripted(t, tr, withTasksExtension())

	require.NoError(t, c.UpdateExtensionTask(t.Context(), extensionTaskID, nil))

	calls := rec.seen()
	require.Len(t, calls, 1)
	var sent struct {
		InputResponses json.RawMessage `json:"inputResponses"`
	}
	require.NoError(t, json.Unmarshal(calls[0].params, &sent))
	assert.JSONEq(t, `{}`, string(sent.InputResponses))
}

// The empty answers of tasks/cancel and tasks/update are judged like any
// result: a resultType the request may not be answered with, or a body that
// is not an object, is an error.
func TestCancelAndUpdateExtensionTaskJudgeTheirAnswer(t *testing.T) {
	calls := map[string]func(c *Client) error{
		"cancel": func(c *Client) error { return c.CancelExtensionTask(t.Context(), extensionTaskID) },
		"update": func(c *Client) error { return c.UpdateExtensionTask(t.Context(), extensionTaskID, nil) },
	}
	answers := []struct {
		name   string
		result string
		check  func(t *testing.T, err error)
	}{
		{"complete", `{"resultType":"complete"}`, func(t *testing.T, err error) { assert.NoError(t, err) }},
		{"empty", `{}`, func(t *testing.T, err error) { assert.NoError(t, err) }},
		{"a task", `{"resultType":"task"}`, func(t *testing.T, err error) { assert.ErrorIs(t, err, errUnacceptedResultType) }},
		{"not an object", `"nope"`, func(t *testing.T, err error) { assert.ErrorContains(t, err, "unmarshal") }},
	}
	for name, call := range calls {
		for _, answer := range answers {
			t.Run(name+"/"+answer.name, func(t *testing.T) {
				tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
					return resultResponse(request, answer.result), nil
				})}
				c := connectScripted(t, tr, withTasksExtension())

				answer.check(t, call(c))
			})
		}
	}
}

// Cancelling the context ends the wait with the context's error and sends
// nothing: no tasks/cancel, and no notification. Whether the task stops is the
// caller's to decide.
func TestWaitForExtensionTaskSendsNothingWhenTheContextEnds(t *testing.T) {
	t.Run("while waiting between polls", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rec := &taskRecorder{answer: func(int) string { return stateJSON("working", new(int64(60_000)), "") }}
			tr := &scriptedTransport{respond: modernScript(rec.respond)}
			c := connectScripted(t, tr, withTasksExtension())
			done := make(chan error, 1)
			go func() {
				_, err := c.WaitForExtensionTask(ctx, extensionTaskID, time.Second)
				done <- err
			}()

			synctest.Wait()
			cancel()

			err := <-done
			require.ErrorIs(t, err, context.Canceled)
			calls := rec.seen()
			require.Len(t, calls, 1, "one poll, then the wait")
			assert.Equal(t, string(mcp.MethodTasksGet), calls[0].method)
			assert.NotContains(t, tr.sentMethods(), string(mcp.MethodTasksCancel))
			assert.Empty(t, tr.sentNotifications())
		})
	})

	t.Run("before the first poll", func(t *testing.T) {
		rec := &taskRecorder{answer: func(int) string { return stateJSON("working", nil, "") }}
		tr := &scriptedTransport{respond: modernScript(rec.respond)}
		c := connectScripted(t, tr, withTasksExtension())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := c.WaitForExtensionTask(ctx, extensionTaskID, time.Second)

		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, rec.seen())
		assert.Empty(t, tr.sentNotifications())
	})
}

// WaitForExtensionTask checks its fallback like WaitForTask, and refuses a
// server interval that overflows a time.Duration.
func TestWaitForExtensionTaskChecksTheIntervals(t *testing.T) {
	t.Run("a non-positive fallback", func(t *testing.T) {
		tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			return resultResponse(request, stateJSON("working", nil, "")), nil
		})}
		c := connectScripted(t, tr, withTasksExtension())
		before := len(tr.sentMethods())
		ctx := safetyNet(t)

		_, err := c.WaitForExtensionTask(ctx, extensionTaskID, 0)

		require.ErrorIs(t, err, ErrInvalidFallbackPollInterval)
		assert.Len(t, tr.sentMethods(), before)
	})

	t.Run("an overflowing server interval", func(t *testing.T) {
		tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			return resultResponse(request, stateJSON("working", new(int64(1<<62)), "")), nil
		})}
		c := connectScripted(t, tr, withTasksExtension())
		ctx := safetyNet(t)

		_, err := c.WaitForExtensionTask(ctx, extensionTaskID, time.Second)

		require.ErrorIs(t, err, ErrServerPollIntervalOverflow)
	})
}

// Today's 2025-11-25 task methods speak shapes a 2026-07-28 server no longer
// answers, so on such a connection each returns an error naming the
// extension's methods, before sending anything. On the handshake era they are
// untouched.
func TestLegacyTaskMethodsAreRefusedOnTheModernEra(t *testing.T) {
	calls := map[string]func(c *Client) error{
		"GetTask": func(c *Client) error {
			_, err := c.GetTask(t.Context(), mcp.GetTaskRequest{Params: mcp.GetTaskParams{TaskId: "t"}})
			return err
		},
		"CancelTask": func(c *Client) error {
			_, err := c.CancelTask(t.Context(), mcp.CancelTaskRequest{Params: mcp.CancelTaskParams{TaskId: "t"}})
			return err
		},
		"WaitForTask": func(c *Client) error {
			_, err := c.WaitForTask(t.Context(), mcp.GetTaskRequest{Params: mcp.GetTaskParams{TaskId: "t"}}, time.Second)
			return err
		},
		"TaskResult": func(c *Client) error {
			_, err := c.TaskResult(t.Context(), mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: "t"}})
			return err
		},
		"ListTasks": func(c *Client) error {
			_, err := c.ListTasks(t.Context(), mcp.ListTasksRequest{})
			return err
		},
		"ListTasksByPage": func(c *Client) error {
			_, err := c.ListTasksByPage(t.Context(), mcp.ListTasksRequest{})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name+"/modern", func(t *testing.T) {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return resultResponse(request, `{"tasks":[]}`), nil
			})}
			c := connectScripted(t, tr, withTasksExtension())
			before := len(tr.sentMethods())

			err := call(c)

			require.ErrorIs(t, err, ErrLegacyTaskMethod)
			assert.ErrorContains(t, err, name+" (", "the error names the method called")
			for _, method := range []string{"GetExtensionTask", "CancelExtensionTask", "UpdateExtensionTask", "WaitForExtensionTask"} {
				assert.ErrorContains(t, err, method)
			}
			assert.Len(t, tr.sentMethods(), before, "nothing was sent")
		})
		t.Run(name+"/handshake era", func(t *testing.T) {
			tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				if request.Method == string(mcp.MethodInitialize) {
					return resultResponse(request, legacyInitializeResult), nil
				}
				return resultResponse(request, `{"taskId":"t","status":"completed","tasks":[],"createdAt":"x","lastUpdatedAt":"x","ttl":null}`), nil
			}}
			c := connectScripted(t, tr, WithLegacyProtocolOnly())

			err := call(c)

			assert.NotErrorIs(t, err, ErrLegacyTaskMethod)
			assert.NoError(t, err)
			assert.Greater(t, len(tr.sentMethods()), 1, "the request was sent")
		})
	}
}

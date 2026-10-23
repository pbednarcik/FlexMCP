package server

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// raisedBy runs f and returns what it panicked with, nil when it returned.
func raisedBy(f func()) (raised any) {
	defer func() { raised = recover() }()
	f()
	return nil
}

// A panic in the validator that no unreadable number raised is raised again,
// though the tree holds such a number, with its value and the stack it was
// raised at. A nil schema makes Validate panic.
func TestValidateTreeRaisesAPanicNoNumberRaised(t *testing.T) {
	for name, value := range map[string]any{
		"with an unreadable number": map[string]any{"a": json.Number("1"), "z": json.Number(outOfRangeNumber)},
		"without":                   map[string]any{"a": json.Number("1")},
	} {
		t.Run(name, func(t *testing.T) {
			raised := raisedBy(func() { _ = validateTree(nil, value) })
			require.NotNil(t, raised, "validateTree returned")
			err, ok := raised.(error)
			require.True(t, ok, "raised %T", raised)
			_, original := errors.AsType[runtime.Error](err)
			assert.True(t, original, "the original panic value: %v", err)
			assert.Contains(t, err.Error(), "jsonschema/v6.(*Schema).Validate", "the stack it was raised at")
		})
	}
}

// The decision compiling shares: the number raised the panic only when the
// run again, every unreadable number read as 0 in a copy, raises none.
func TestBlameUnreadableNumber(t *testing.T) {
	doc := map[string]any{"maximum": json.Number(outOfRangeNumber), "minimum": json.Number("1")}
	original := &recoveredPanic{value: "not the number", stack: []byte("the stack")}
	var ranOn any
	blamed := blameUnreadableNumber(doc, original, func(zeroed any) *recoveredPanic {
		ranOn = zeroed
		return nil
	})
	require.NotNil(t, blamed)
	assert.Equal(t, "/maximum: number "+outOfRangeNumber+" is out of range", blamed.Error())
	assert.Equal(t, map[string]any{"maximum": json.Number("0"), "minimum": json.Number("1")}, ranOn)
	assert.Equal(t, json.Number(outOfRangeNumber), doc["maximum"], "the tree itself is left as it was")

	raised := raisedBy(func() {
		_ = blameUnreadableNumber(doc, original, func(any) *recoveredPanic { return &recoveredPanic{value: "again"} })
	})
	assert.Same(t, original, raised, "a panic the run again raises too is raised again")
}

// panicsWhenEncoded is a schema member whose encoding panics, as a bug in a
// caller's type would.
type panicsWhenEncoded struct{}

func (panicsWhenEncoded) MarshalJSON() ([]byte, error) { panic("not a number") }

// A registration that panics for any reason leaves the tools lock free.
func TestARegistrationPanicReleasesToolsMu(t *testing.T) {
	tool := mcp.NewTool("odd")
	tool.InputSchema.Properties = map[string]any{"x": panicsWhenEncoded{}}
	taskHandler := func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) { return nil, nil }
	for name, register := range map[string]func(*MCPServer){
		"AddTools":     func(s *MCPServer) { s.AddTools(ServerTool{Tool: tool, Handler: okHandler}) },
		"AddTaskTools": func(s *MCPServer) { s.AddTaskTools(ServerTaskTool{Tool: tool, Handler: taskHandler}) },
		"SetTools":     func(s *MCPServer) { s.SetTools(ServerTool{Tool: tool, Handler: okHandler}) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
			assert.PanicsWithValue(t, "not a number", func() { register(srv) })
			require.True(t, srv.toolsMu.TryLock(), "toolsMu is held after the panic")
			srv.toolsMu.Unlock()
		})
	}
}

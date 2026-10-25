package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A task belongs to the session that created it: a task ID is the bearer
// token of its stored state, and no other session learns one or uses one
// (Tasks.md row 25).

func newTaskSession(id string) fakeSess {
	return fakeSess{sessionID: id, notifyChan: make(chan mcp.JSONRPCNotification, 10)}
}

// A task's status is told to the session that created it, and to no other;
// a task created without a session has nobody to tell.
func TestTaskStatusIsToldToTheOwningSessionOnly(t *testing.T) {
	server := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	owner, other := newTaskSession("owner"), newTaskSession("other")
	require.NoError(t, server.RegisterSession(t.Context(), owner))
	require.NoError(t, server.RegisterSession(t.Context(), other))

	entry, err := server.createTask(server.WithContext(t.Context(), owner), "owned", "tool", nil, nil)
	require.NoError(t, err)
	server.completeTask(entry, "result", nil)
	select {
	case n := <-owner.notifyChan:
		assert.Equal(t, mcp.MethodNotificationTasksStatus, n.Method)
		assert.Equal(t, "owned", n.Params.AdditionalFields["taskId"])
	case <-time.After(time.Second):
		t.Fatal("the owning session was not told")
	}
	select {
	case n := <-other.notifyChan:
		t.Fatalf("another session was told of the task: %v", n.Params.AdditionalFields)
	case <-time.After(100 * time.Millisecond):
	}

	orphan, err := server.createTask(t.Context(), "orphan", "tool", nil, nil)
	require.NoError(t, err)
	server.completeTask(orphan, "result", nil)
	select {
	case n := <-owner.notifyChan:
		t.Fatalf("a session was told of a sessionless task: %v", n.Params.AdditionalFields)
	case n := <-other.notifyChan:
		t.Fatalf("a session was told of a sessionless task: %v", n.Params.AdditionalFields)
	case <-time.After(100 * time.Millisecond):
	}
}

// A task is reachable by its session only. A caller with no session, as
// every streamable-HTTP request is, does not reach a session's task, and a
// session does not reach a task created with none: there the ID alone is
// the bearer token.
func TestTaskIsReachableByItsSessionOnly(t *testing.T) {
	server := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	owner := newTaskSession("owner")
	require.NoError(t, server.RegisterSession(t.Context(), owner))
	ownerCtx := server.WithContext(t.Context(), owner)
	_, err := server.createTask(ownerCtx, "owned", "tool", nil, nil)
	require.NoError(t, err)
	_, err = server.createTask(t.Context(), "sessionless", "tool", nil, nil)
	require.NoError(t, err)

	for _, tt := range []struct {
		name      string
		ctx       context.Context
		taskID    string
		reachable bool
	}{
		{"its session reaches a session's task", ownerCtx, "owned", true},
		{"no session does not reach a session's task", t.Context(), "owned", false},
		{"another session does not reach a session's task", server.WithContext(t.Context(), newTaskSession("other")), "owned", false},
		{"no session reaches a sessionless task", t.Context(), "sessionless", true},
		{"a session does not reach a sessionless task", ownerCtx, "sessionless", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := server.getTask(tt.ctx, tt.taskID)
			if tt.reachable {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, "task not found")
			require.EqualError(t, server.cancelTask(tt.ctx, tt.taskID), "task not found")
		})
	}
}

// An expired task is expired to its session and unknown to any other: the
// tombstone tells no one else that the ID ever existed.
func TestExpiredTaskIsKnownToItsSessionOnly(t *testing.T) {
	server := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	owner := newTaskSession("owner")
	require.NoError(t, server.RegisterSession(t.Context(), owner))
	ownerCtx := server.WithContext(t.Context(), owner)
	ttl := int64(20)
	_, err := server.createTask(ownerCtx, "gone", "tool", &ttl, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, _, err := server.getTask(ownerCtx, "gone")
		return err != nil && err.Error() == "task has expired"
	}, time.Second, 5*time.Millisecond)
	_, _, err = server.getTask(t.Context(), "gone")
	require.EqualError(t, err, "task not found")
}

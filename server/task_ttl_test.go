package server

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A task is kept for at most the server's TTL: the request's TTL when it
// asks for less, the server's otherwise, and the task reports the TTL it got.
func TestTaskTTL_IsTheServersUnlessTheRequestAsksForLess(t *testing.T) {
	ttlOf := func(server *MCPServer, requested *int64) *int64 {
		t.Helper()
		entry, err := server.createTask(t.Context(), "task", "tool", requested, nil)
		require.NoError(t, err)
		server.tasksMu.RLock()
		defer server.tasksMu.RUnlock()
		return entry.task.TTL
	}

	t.Run("none asked: the server's", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0", WithTaskTTL(30*time.Millisecond))
		require.Equal(t, int64(30), *ttlOf(server, nil))
	})
	t.Run("more asked: the server's", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0", WithTaskTTL(30*time.Millisecond))
		require.Equal(t, int64(30), *ttlOf(server, new(int64(60000))))
	})
	t.Run("less asked: the request's", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0", WithTaskTTL(30*time.Millisecond))
		require.Equal(t, int64(10), *ttlOf(server, new(int64(10))))
	})
	t.Run("no option and none asked: no expiry", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0")
		require.Nil(t, ttlOf(server, nil))
	})
	t.Run("no server TTL and none asked: no expiry", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0", WithTaskTTL(0))
		require.Nil(t, ttlOf(server, nil))
	})
	t.Run("no server TTL and zero or less asked: no expiry, and none reported", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0", WithTaskTTL(0))
		require.Nil(t, ttlOf(server, new(int64(0))))
		require.Nil(t, ttlOf(server, new(int64(-5))))
	})
	t.Run("a server TTL under a millisecond is a millisecond, not none", func(t *testing.T) {
		server := NewMCPServer("test", "1.0.0", WithTaskTTL(500*time.Microsecond))
		require.Equal(t, int64(1), *ttlOf(server, nil))
	})
}

// A task with no requested TTL expires after the server's.
func TestTaskTTL_ATaskWithNoRequestedTTLExpires(t *testing.T) {
	server := NewMCPServer("test", "1.0.0", WithTaskTTL(20*time.Millisecond))
	_, err := server.createTask(t.Context(), "task", "tool", nil, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, _, err := server.getTask(t.Context(), "task")
		return err != nil && err.Error() == "task has expired"
	}, time.Second, 5*time.Millisecond)
}

// A requested TTL beyond what a duration can hold is cut to the longest,
// not wrapped into one that expires the task at once.
func TestTaskTTL_ARequestBeyondTheLongestDurationIsCut(t *testing.T) {
	server := NewMCPServer("test", "1.0.0", WithTaskTTL(0))
	entry, err := server.createTask(t.Context(), "task", "tool", new(int64(math.MaxInt64)), nil)
	require.NoError(t, err)
	server.tasksMu.RLock()
	ttl := *entry.task.TTL
	server.tasksMu.RUnlock()
	assert.Equal(t, int64(math.MaxInt64/time.Millisecond), ttl)

	time.Sleep(50 * time.Millisecond)
	_, _, err = server.getTask(t.Context(), "task")
	require.NoError(t, err, "a task with the longest TTL expired at once")
}

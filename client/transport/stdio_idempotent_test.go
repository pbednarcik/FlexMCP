package transport

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestStdio_StartIdempotency tests that calling Start() multiple times is safe
func TestStdio_StartIdempotency(t *testing.T) {
	tempFile, err := os.CreateTemp(t.TempDir(), "mockstdio_server")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tempFile.Close()
	mockServerPath := tempFile.Name() + ".exe"

	if compileErr := compileTestServer(mockServerPath); compileErr != nil {
		t.Fatalf("Failed to compile mock server: %v", compileErr)
	}

	stdio := NewStdio(mockServerPath, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// First Start() - should succeed
	err = stdio.Start(ctx)
	if err != nil {
		t.Fatalf("First Start() failed: %v", err)
	}
	defer stdio.Close()

	// Second Start() - should be idempotent (no error, no double-start)
	err = stdio.Start(ctx)
	if err != nil {
		t.Errorf("Second Start() should be idempotent, got error: %v", err)
	}

	// Third Start() - should still be idempotent
	err = stdio.Start(ctx)
	if err != nil {
		t.Errorf("Third Start() should be idempotent, got error: %v", err)
	}

	// Verify transport still works after multiple Start() calls
	request := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      mcp.NewRequestId(int64(1)),
		Method:  "ping",
	}

	_, err = stdio.SendRequest(ctx, request)
	if err != nil {
		t.Errorf("Transport should still work after multiple Start() calls: %v", err)
	}
}

// TestStdio_StartFailureReset tests that a failed Start() can be retried
func TestStdio_StartFailureReset(t *testing.T) {
	// First attempt with invalid command - should fail
	stdio := NewStdio("nonexistent_command_xyz", nil)

	ctx := t.Context()
	err := stdio.Start(ctx)
	if err == nil {
		t.Fatal("Expected Start() to fail with invalid command")
	}

	// Verify started flag was reset on failure
	stdio.startedMu.Lock()
	started := stdio.started
	stdio.startedMu.Unlock()

	if started {
		t.Error("Started flag should be false after failed Start()")
	}

	// Should be able to retry (won't succeed with same bad command, but won't panic)
	err = stdio.Start(ctx)
	if err == nil {
		t.Fatal("Expected second Start() to also fail with invalid command")
	}
}

// TestStdio_StartWithOptions_Idempotent tests idempotency with custom options
func TestStdio_StartWithOptions_Idempotent(t *testing.T) {
	tempFile, err := os.CreateTemp(t.TempDir(), "mockstdio_server")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	tempFile.Close()
	mockServerPath := tempFile.Name() + ".exe"

	if compileErr := compileTestServer(mockServerPath); compileErr != nil {
		t.Fatalf("Failed to compile mock server: %v", compileErr)
	}

	cmdFuncCallCount := 0
	cmdFunc := func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
		cmdFuncCallCount++
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Env = append(os.Environ(), env...)
		return cmd, nil
	}

	stdio := NewStdioWithOptions(mockServerPath, nil, nil, WithCommandFunc(cmdFunc))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// First Start()
	err = stdio.Start(ctx)
	if err != nil {
		t.Fatalf("First Start() failed: %v", err)
	}
	defer stdio.Close()

	// Second Start() - cmdFunc should NOT be called again
	err = stdio.Start(ctx)
	if err != nil {
		t.Errorf("Second Start() should be idempotent, got error: %v", err)
	}

	// Verify cmdFunc was only called once
	if cmdFuncCallCount != 1 {
		t.Errorf("Expected cmdFunc to be called once, got %d times", cmdFuncCallCount)
	}
}

// A closed transport stays closed: Start after Close spawns nothing, as the
// SSE and in-process transports refuse too, since no Close is left to reap
// the child.
func TestStdio_StartAfterCloseIsRefused(t *testing.T) {
	var spawned int
	stdio := NewStdioWithOptions("nonexistent_command_xyz", nil, nil, WithCommandFunc(
		func(context.Context, string, []string, []string) (*exec.Cmd, error) {
			spawned++
			return nil, errors.New("spawn refused")
		}))
	require.NoError(t, stdio.Close())

	require.ErrorIs(t, stdio.Start(t.Context()), ErrTransportClosed)
	assert.Zero(t, spawned, "a closed transport spawned a child")
}

// A Start that finds another in progress waits for it, rather than report a
// transport started that is still spawning.
func TestStdio_ConcurrentStartWaitsForTheFirst(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	stdio := NewStdioWithOptions("nonexistent_command_xyz", nil, nil, WithCommandFunc(
		func(context.Context, string, []string, []string) (*exec.Cmd, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return nil, errors.New("spawn refused")
		}))

	first := make(chan error, 1)
	go func() { first <- stdio.Start(t.Context()) }()
	<-entered
	second := make(chan error, 1)
	go func() { second <- stdio.Start(t.Context()) }()
	select {
	case err := <-second:
		t.Fatalf("the second Start returned %v while the first was still spawning", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.Error(t, <-first)
	require.Error(t, <-second, "the first spawn failed, so the second tries its own")
}

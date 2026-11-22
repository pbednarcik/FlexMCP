package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strconv"
	"testing"
	"time"
)

// A writer a cancelled Listen left stuck in a write sends nothing more once
// that write returns, and the next Listen on the server starts after it ends:
// the two never share the server's state.

// cancelledListenWithHeldWriter runs a Listen on s whose writer is held in
// its first write to stdout, then cancels that Listen and waits for it to
// return, leaving the writer held.
func cancelledListenWithHeldWriter(t *testing.T, s *StdioServer, stdout io.Writer, holding <-chan struct{}) {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	t.Cleanup(func() { _ = stdinReader.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	listened := make(chan struct{})
	go func() {
		defer close(listened)
		_ = s.Listen(ctx, stdinReader, stdout)
	}()
	go func() { _, _ = stdinWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")) }()
	awaitClosed(t, holding, "the first Listen's writer to be held")
	cancel()
	awaitClosed(t, listened, "the first Listen to return on its cancel")
}

// letGoOnceRead closes release, letting the held writer go, once the second
// Listen has read its first request (read is closed), so that what the
// writer does next meets that Listen running; or after a moment, as a Listen
// that waits for the held writer reads nothing before that writer ends.
func letGoOnceRead(read <-chan struct{}, release chan struct{}) {
	select {
	case <-read:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
}

func awaitClosed(t *testing.T, c <-chan struct{}, describe string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", describe)
	}
}

// listenAgain runs a second Listen on s, on stdio of its own, and returns
// its stdin, the messages it writes, and its result once it returns.
func listenAgain(t *testing.T, ctx context.Context, s *StdioServer) (stdin io.Writer, messages <-chan stdioLine, result <-chan error) {
	t.Helper()
	stdoutReader, stdoutWriter := io.Pipe()
	lines := make(chan stdioLine, 256)
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		for scanner.Scan() {
			var line stdioLine
			if json.Unmarshal(scanner.Bytes(), &line) == nil {
				lines <- line
			}
		}
	}()
	stdinReader, stdinWriter := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Listen(ctx, stdinReader, stdoutWriter) }()
	t.Cleanup(func() {
		cancel()
		_ = stdinReader.Close()
		_ = stdoutReader.Close()
	})
	return stdinWriter, lines, done
}

func TestStdioServer_AWriterLeftByACancelledListenWritesNoLaterResponse(t *testing.T) {
	s := NewStdioServer(NewMCPServer("test", "1.0.0"))
	s.SetErrorLogger(log.New(io.Discard, "", 0))
	held := &heldWriter{holding: make(chan struct{}), release: make(chan struct{}), out: make(chan stdioLine, 256)}
	cancelledListenWithHeldWriter(t, s, held, held.holding)

	stdin, messages, _ := listenAgain(t, t.Context(), s)
	const n = 50
	read := make(chan struct{})
	go func() {
		for id := 2; id < 2+n; id++ {
			_, err := stdin.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"ping"}` + "\n"))
			if id == 2 {
				close(read)
			}
			if err != nil {
				return
			}
		}
	}()
	letGoOnceRead(read, held.release)
	answered := 0
	for answered < n {
		select {
		case <-messages:
			answered++
		case line := <-held.out:
			if string(line.ID) != "1" {
				t.Fatalf("the first Listen's stdout got the second Listen's response to %s", line.ID)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out: the second Listen answered %d of %d", answered, n)
		}
	}
}

// failingHeldWriter holds the first write until release is closed, then
// fails it, as a stdout whose reader went away.
type failingHeldWriter struct{ holding, release chan struct{} }

var errStdoutGone = errors.New("the first stdout is gone")

func (w *failingHeldWriter) Write([]byte) (int, error) {
	close(w.holding)
	<-w.release
	return 0, errStdoutGone
}

func TestStdioServer_AWriterLeftByACancelledListenFailsNoLaterListen(t *testing.T) {
	s := NewStdioServer(NewMCPServer("test", "1.0.0"))
	s.SetErrorLogger(log.New(io.Discard, "", 0))
	held := &failingHeldWriter{holding: make(chan struct{}), release: make(chan struct{})}
	cancelledListenWithHeldWriter(t, s, held, held.holding)

	stdin, messages, result := listenAgain(t, t.Context(), s)
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"))
	}()
	letGoOnceRead(read, held.release)
	select {
	case line := <-messages:
		if string(line.ID) != "2" {
			t.Fatalf("the second Listen wrote %s, want the answer to 2", line.raw)
		}
	case err := <-result:
		t.Fatalf("the second Listen ended with %v when the first writer's write failed", err)
	case <-time.After(3 * time.Second):
		t.Fatal("the second Listen did not answer")
	}
}

func TestStdioServer_AListenWaitingOnAHeldWriterEndsOnItsCancel(t *testing.T) {
	s := NewStdioServer(NewMCPServer("test", "1.0.0"))
	s.SetErrorLogger(log.New(io.Discard, "", 0))
	held := &heldWriter{holding: make(chan struct{}), release: make(chan struct{}), out: make(chan stdioLine, 1)}
	t.Cleanup(func() { close(held.release) })
	cancelledListenWithHeldWriter(t, s, held, held.holding)

	ctx, cancel := context.WithCancel(t.Context())
	_, _, result := listenAgain(t, ctx, s)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the second Listen returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the second Listen did not return on its cancel while the first writer was held")
	}
}

func TestStdioServer_AWriterLeftByACancelledListenWritesNothingMore(t *testing.T) {
	s := NewStdioServer(NewMCPServer("test", "1.0.0"))
	s.SetErrorLogger(log.New(io.Discard, "", 0))
	held := &heldWriter{holding: make(chan struct{}), release: make(chan struct{}), out: make(chan stdioLine, 8)}
	stdinReader, stdinWriter := io.Pipe()
	t.Cleanup(func() { _ = stdinReader.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	listened := make(chan struct{})
	go func() {
		defer close(listened)
		_ = s.Listen(ctx, stdinReader, held)
	}()
	go func() {
		_, _ = stdinWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"))
	}()
	awaitClosed(t, held.holding, "the writer to be held")
	awaitQueued(t, s, 1, "the second response")
	cancel()
	awaitClosed(t, listened, "Listen to return on its cancel")
	writerDone := s.writerDone
	close(held.release)
	awaitClosed(t, writerDone, "the writer to end once its write returned")
	close(held.out)
	var written []string
	for line := range held.out {
		written = append(written, string(line.ID))
	}
	// Requests are served on goroutines of their own, so either response may
	// be the held write; the other waited in the queue.
	if len(written) != 1 {
		t.Fatalf("the cancelled Listen's writer wrote %v, want only the held write", written)
	}
}

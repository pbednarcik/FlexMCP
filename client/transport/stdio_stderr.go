package transport

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
)

// stderrBuffer is a bounded, drop-oldest buffer of the subprocess's stderr
// output. The transport writes into it continuously so the OS pipe never
// fills up and blocks the child, while callers can still read recent stderr
// through Stderr(). Reads block until data is available or the stream ends.
type stderrBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  bytes.Buffer
	done bool
}

// newStderrBuffer returns an empty, open stderr buffer ready for writing.
func newStderrBuffer() *stderrBuffer {
	b := &stderrBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends p to the buffer, dropping the oldest data when the buffer is
// full. It never blocks, so a child writing to stderr can never deadlock the
// transport.
func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return 0, io.ErrClosedPipe
	}
	// Report the original length per the io.Writer contract even when the
	// tail truncation below drops the leading bytes.
	written := len(p)
	if len(p) > stderrBufferSize {
		p = p[len(p)-stderrBufferSize:]
	}
	if overflow := b.buf.Len() + len(p) - stderrBufferSize; overflow > 0 {
		b.buf.Next(overflow)
	}
	b.buf.Write(p)
	b.cond.Broadcast()
	return written, nil
}

// Read returns buffered data, blocking until data is available or the stream
// is marked done.
func (b *stderrBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.done {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		return 0, io.EOF
	}
	return b.buf.Read(p)
}

// Close marks the stream as ended and wakes up blocked readers.
func (b *stderrBuffer) Close() error {
	b.mu.Lock()
	b.done = true
	b.cond.Broadcast()
	b.mu.Unlock()
	return nil
}

// startStderrDrain starts the background goroutine that keeps the subprocess
// stderr pipe empty. An unread pipe holds only about 64KB; once it fills, the
// child blocks inside write(2) and stops answering on stdout, deadlocking
// every request (including Ping) with no error. The output is written to the
// bounded ring buffer (readable through Stderr()) and mirrored to
// stderrWriter, which defaults to io.Discard.
func (c *Stdio) startStderrDrain(stderr io.ReadCloser) {
	c.stderrRing = newStderrBuffer()
	go drainStderrIntoRing(c.stderrRing, stderr, c.stderrWriter, c.logger)
}

// drainStderrIntoRing copies src into the ring buffer until the stream ends,
// closing the ring afterwards to wake readers blocked in Stderr().Read.
//
// The drain must never stop while the child runs: a full OS pipe blocks the
// child inside write(2) and deadlocks every request. When dest is nil the
// stream is drained directly into the ring without any mirroring overhead;
// otherwise mirroring is decoupled through a bounded queue so a failing or
// blocked writer cannot stop the pipe drain: once the queue fills up (or the
// mirror goroutine exits) further chunks are dropped and the loss is logged
// at debug level. The mirror goroutine is intentionally not waited for: a
// writer stuck inside Write would otherwise delay ring Close and leave
// Stderr() readers blocked.
func drainStderrIntoRing(ring *stderrBuffer, src io.ReadCloser, dest io.Writer, logger *slog.Logger) {
	defer func() {
		_ = ring.Close()
	}()

	// No custom destination configured: drain directly without spawning a
	// mirror goroutine or allocating mirror chunks.
	if dest == nil {
		_, _ = io.Copy(ring, src)
		return
	}

	mirror := make(chan []byte, 8)
	go func() {
		for chunk := range mirror {
			if _, err := dest.Write(chunk); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := ring.Write(buf[:n]); werr != nil {
				break
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case mirror <- chunk:
			default:
				// Mirror is full or gone; dropping the
				// chunk keeps the pipe draining.
				if logger != nil {
					logger.Debug("stderr mirror queue full; dropping chunk", "bytes", n)
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	close(mirror)
}

// Stderr returns a reader for the stderr output of the subprocess.
// This can be used to capture error messages or logs from the subprocess.
//
// The transport drains the subprocess's stderr into a bounded buffer so the
// OS pipe never fills up and deadlocks the child. The reader returns the most
// recent output; older output is dropped once the buffer is full. To capture
// the full, real-time stream, use WithCommandStderrWriter instead.
func (c *Stdio) Stderr() io.Reader {
	if c.stderrRing != nil {
		return c.stderrRing
	}
	return c.stderr
}

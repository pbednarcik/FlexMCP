package transport

import (
	"bufio"
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/pbednarcik/flexmcp/mcp"
)

// maxRetainedLine is the largest buffer a reader keeps from one message to
// the next: the stdio reader's line, and the SSE reader's line, event name
// and data. One a larger message grew is let go.
const maxRetainedLine = 1 << 20

// readLine reads the next line from r into buf, whose storage it reuses, and
// returns it without its line end. The line lives in buf until the next read.
// A line longer than buf's storage is kept in parts, as bufio's ReadBytes
// keeps it, and joined once at its length, rather than growing buf by every
// part.
func readLine(r *bufio.Reader, buf []byte) ([]byte, error) {
	return readLineMax(r, buf, 0)
}

// readLineMax is readLine refusing a line longer than limit bytes, its line
// end included, with ErrMessageTooLarge: the check comes before each part of
// the line is kept, so a longer line is never held whole. A limit of zero or
// less is no limit.
func readLineMax(r *bufio.Reader, buf []byte, limit int) ([]byte, error) {
	buf = buf[:0]
	var parts [][]byte
	n := 0
	for {
		chunk, err := r.ReadSlice('\n')
		if limit > 0 && n+len(chunk) > limit {
			return buf[:0], ErrMessageTooLarge
		}
		n += len(chunk)
		if errors.Is(err, bufio.ErrBufferFull) {
			if parts == nil && n <= cap(buf) {
				buf = append(buf, chunk...)
			} else {
				parts = append(parts, bytes.Clone(chunk))
			}
			continue
		}
		if parts != nil {
			buf = slices.Grow(buf, n-len(buf))
			for _, part := range parts {
				buf = append(buf, part...)
			}
		}
		buf = append(buf, chunk...)
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// responseKey keys a pending request's response channel: the id's String
// form, made comparable without building it. An integer id, which String
// writes as "int64:N", is n; a string id is its text; any other is its
// String.
type responseKey struct {
	kind uint8
	n    int64
	s    string
}

const (
	intKey = iota + 1
	stringKey
	otherKey
)

// responseKeyOf is id's response key. Two ids share one exactly when their
// String forms are equal.
func responseKeyOf(id mcp.RequestId) responseKey {
	switch v := id.Value().(type) {
	case int64:
		return responseKey{kind: intKey, n: v}
	case string:
		return responseKey{kind: stringKey, s: v}
	}
	s := id.String()
	if digits, ok := strings.CutPrefix(s, "int64:"); ok {
		if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
			return responseKey{kind: intKey, n: n}
		}
	}
	return responseKey{kind: otherKey, s: s}
}

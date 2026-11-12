package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync"
)

// DefaultMaxMessageBytes is the largest message the SSE and Streamable HTTP
// client transports read from a server by default, 32 MiB: a JSON response
// body, or the data of one SSE event.
const DefaultMaxMessageBytes int64 = 32 << 20

// ErrMessageTooLarge is the error of a message from the server over the
// bound. See WithMaxMessageBytes and WithHTTPMaxMessageBytes.
var ErrMessageTooLarge = errors.New("message from the server exceeds the size bound")

// maxErrorBodyBytes is the most of an error response's body the client
// reads, for the error's text.
const maxErrorBodyBytes = 64 << 10

// readErrorBody reads the body r of an error response as io.ReadAll does, up
// to maxErrorBodyBytes: the rest is left unread.
func readErrorBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
}

// boundedReader reads at most n bytes of r. Once they are spent, one byte
// more tells a source that fills the bound from a longer one, which fails
// with ErrMessageTooLarge.
type boundedReader struct {
	r     io.Reader
	n     int64
	probe [1]byte
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.n <= 0 {
		n, err := b.r.Read(b.probe[:])
		if n > 0 {
			return 0, ErrMessageTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.n {
		p = p[:b.n]
	}
	n, err := b.r.Read(p)
	b.n -= int64(n)
	return n, err
}

// boundedReaders keeps the readers decodeBounded wraps an answer in, so that
// reading one allocates none.
var boundedReaders = sync.Pool{New: func() any { return new(boundedReader) }}

// takeBoundedReader is r bounded at limit bytes, from the pool;
// giveBoundedReader puts it back once it is no longer read.
func takeBoundedReader(r io.Reader, limit int64) *boundedReader {
	b := boundedReaders.Get().(*boundedReader)
	b.r, b.n = r, limit
	return b
}

// giveBoundedReader puts b back in the pool, without its source.
func giveBoundedReader(b *boundedReader) {
	b.r = nil
	boundedReaders.Put(b)
}

// decodeBounded decodes the JSON value r holds into v, reading at most limit
// bytes of r: a longer value fails with ErrMessageTooLarge. A limit of zero or
// less is no bound. As json.Decoder does, it reads no further than the value.
func decodeBounded(r io.Reader, limit int64, v any) error {
	if limit <= 0 {
		return json.NewDecoder(r).Decode(v)
	}
	b := takeBoundedReader(r, limit)
	err := json.NewDecoder(b).Decode(v)
	giveBoundedReader(b)
	return err
}

// sseLineSlack is what an SSE line carries besides its value: a field name
// and a line end, so that a data line whose value is exactly the bound fits.
const sseLineSlack = len("event: \r\n")

// sseLineLimit is the longest SSE line, its line end included, read under
// the message bound limit; 0, no limit, for a limit of zero or less.
func sseLineLimit(limit int64) int {
	if limit <= 0 {
		return 0
	}
	return int(min(limit, int64(math.MaxInt-sseLineSlack))) + sseLineSlack
}

// sseDataFits reports whether the value of a data line, after "data:", joins
// onto an event's data within the message bound limit, counted as
// appendSSEDataBytes joins it: the newline before it included. A limit of
// zero or less is no bound.
func sseDataFits(data, line []byte, limit int64) bool {
	if limit <= 0 {
		return true
	}
	n := len(data) + len(bytes.TrimPrefix(line, []byte(" ")))
	if len(data) > 0 {
		n++
	}
	return int64(n) <= limit
}

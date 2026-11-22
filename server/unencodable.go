package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/pbednarcik/flexmcp/mcp"
)

// errNotEncoded marks a message that could not be encoded, such as a tool
// result holding NaN: nothing of it was written, so the transport is as good
// as before. A response is answered with [answerInPlaceOf] instead; any
// other message is dropped. Either way the cause goes to the server's log.
var errNotEncoded = errors.New("message cannot be encoded")

// notEncoded wraps the encoding's error in errNotEncoded.
func notEncoded(err error) error {
	return fmt.Errorf("%w: %w", errNotEncoded, err)
}

// answerInPlaceOf returns the internal error that answers the request
// message answered, when message, a response, could not be encoded: the
// client is not left waiting, and is told no more than a recovered panic
// tells it. A message that answers no request has no stand-in.
func answerInPlaceOf(message any) (mcp.JSONRPCMessage, bool) {
	var id mcp.RequestId
	switch m := message.(type) {
	case mcp.JSONRPCResponse:
		id = m.ID
	case mcp.JSONRPCError:
		id = m.ID
	default:
		return nil, false
	}
	return mcp.JSONRPCError{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      id,
		Error:   mcp.NewJSONRPCErrorDetails(mcp.INTERNAL_ERROR, internalErrorMessage, nil),
	}, true
}

// encodeWriter encodes one message at a time, as json.Encoder does: the
// whole message, newline included, in one write, made only once it is
// encoded. So an Encode error with no write is the encoding's, and nothing of
// the message reached the writer. A transport keeps one, so that encoding
// through it costs nothing more than json.NewEncoder(w).Encode.
type encodeWriter struct {
	w      io.Writer
	header statusWriter // sent status before the first write; nil for none
	status int
	wrote  bool
}

// statusWriter sends an HTTP status.
type statusWriter interface{ WriteHeader(statusCode int) }

// encode writes message to w. With header, status is sent first, and only
// with the message's bytes: a message that cannot be encoded leaves the
// status to the stand-in that answers in its place. A message that cannot
// be encoded answers an error wrapping errNotEncoded, and writes nothing.
func (e *encodeWriter) encode(w io.Writer, header statusWriter, status int, message any) error {
	e.w, e.header, e.status, e.wrote = w, header, status, false
	defer func() { e.w, e.header = nil, nil }()
	err := json.NewEncoder(e).Encode(message)
	if err != nil && !e.wrote {
		return notEncoded(err)
	}
	return err
}

func (e *encodeWriter) Write(p []byte) (int, error) {
	if !e.wrote && e.header != nil {
		e.header.WriteHeader(e.status)
	}
	e.wrote = true
	return e.w.Write(p)
}

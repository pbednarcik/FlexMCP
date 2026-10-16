package server

import (
	"context"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
)

// inflightID keys a request in flight: its session and its JSON-RPC id, the
// id with its type, so the string "7" and the integer 7 are two requests. A
// struct, not a string joined from the two: a session ID may hold any
// character without colliding with another session's, and no string is
// built per request.
type inflightID struct {
	session string
	id      string
}

// inflightRequest is a request in flight, filed under its inflightID. Its
// cancel may be wrapped while it runs (closeStreamOnCancel), so the entry is
// what is filed, not the func: the request's deferred removal takes out its
// own entry and no other, which leaves a request that reused the id after a
// cancel filed (CompareAndDelete).
type inflightRequest struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

// fire cancels the request.
func (r *inflightRequest) fire() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	cancel()
}

// wrapCancel makes wrap(cancel) the request's cancel.
func (r *inflightRequest) wrapCancel(wrap func(context.CancelFunc) context.CancelFunc) {
	r.mu.Lock()
	r.cancel = wrap(r.cancel)
	r.mu.Unlock()
}

// inflightKey returns the key a request is filed under, scoped to its
// session so one client cannot cancel another's request. A session with no
// ID, a modern HTTP request's ephemeral one, has no scope to offer: ok is
// false, and closing the request's stream is its cancel.
func inflightKey(ctx context.Context, requestID any) (key inflightID, ok bool) {
	id := mcp.NewRequestId(requestID).String()
	if session := ClientSessionFromContext(ctx); session != nil {
		if session.SessionID() == "" {
			return inflightID{}, false
		}
		return inflightID{session: session.SessionID(), id: id}, true
	}
	return inflightID{id: id}, true
}

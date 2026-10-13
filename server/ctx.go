package server

import "context"

type contextKey int

const (
	// This const is used as key for context value lookup
	requestHeader contextKey = iota
)

// messageContext is the context a message is handled in, made in one
// allocation: the server's layer, which the hooks see, and the protocol
// info's, which wraps it once the info is read. A layer is never changed
// once handed out.
type messageContext struct {
	server   serverContext
	protocol protocolContext
	info     RequestProtocolInfo
}

// serverContext carries the server handling the message, as [ServerFromContext]
// finds it; every other value, and the cancellation, are its parent's.
type serverContext struct {
	context.Context
	srv *MCPServer
}

func (c *serverContext) Value(key any) any {
	if _, ok := key.(serverKey); ok {
		return c.srv
	}
	return c.Context.Value(key)
}

// protocolContext carries the request's protocol info, as
// [RequestProtocolInfoFromContext] finds it; every other value, and the
// cancellation, are its parent's.
type protocolContext struct {
	context.Context
	info *RequestProtocolInfo
}

func (c *protocolContext) Value(key any) any {
	if _, ok := key.(requestProtocolKey); ok {
		return c.info
	}
	return c.Context.Value(key)
}

package transport

// queuedResponse takes a response already queued on responses, without
// waiting. A wait for a response that ends for another reason (the caller's
// cancel, Close, a timeout) looks here first: a response that arrived is the
// answer, and a select takes either of two ready cases at random. ok is false
// when none is queued, or when responses is closed.
func queuedResponse(responses <-chan *JSONRPCResponse) (response *JSONRPCResponse, ok bool) {
	select {
	case response, ok = <-responses:
		return response, ok
	default:
		return nil, false
	}
}

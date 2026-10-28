package server

import (
	"encoding/json"

	"github.com/pbednarcik/flexmcp/internal/argsdecode"
	"github.com/pbednarcik/flexmcp/mcp"
)

// decodeToolCall decodes message, a tools/call request, into request. A call
// to a tool registered with ServerTool.ArgumentsOnDemand keeps its arguments
// undecoded, for the getters; on a server none of whose tools ever did, the
// call is decoded as json.Unmarshal decodes it, at the cost of an atomic load.
func (s *MCPServer) decodeToolCall(message json.RawMessage, request *mcp.CallToolRequest) error {
	if !s.argumentsOnDemand.Load() {
		return json.Unmarshal(message, request)
	}
	argsdecode.SetOnDemand(&request.Params, s.readsArgumentsOnDemand)
	err := json.Unmarshal(message, request)
	argsdecode.SetOnDemand(&request.Params, nil)
	return err
}

// mayLeaveArgumentsToTheGetters reports whether params, a call's, may carry
// arguments its decode left to the getters: sent, not in Arguments, on a
// server some tool of which opted in.
func (s *MCPServer) mayLeaveArgumentsToTheGetters(params *mcp.CallToolParams) bool {
	return params.Arguments == nil && len(params.RawArguments) > 0 && s.argumentsOnDemand.Load()
}

// argumentsAtOnce returns params with the Arguments a call decoded at once
// carries, when its decode left them to the getters, for a tool found, when
// the call reaches it, not to read them on demand: one registered again
// without ServerTool.ArgumentsOnDemand since the call was decoded. The
// dispatcher's copy of the call is given them too (settleToolCall). Only the
// calls that reach it move params to the heap.
func argumentsAtOnce(params mcp.CallToolParams) mcp.CallToolParams {
	argsdecode.AtOnce(&params)
	return params
}

// decodedArguments returns the Arguments a call decoded at once carries, for
// input validation, which reads arguments that are not an object as such a
// call carries them; the call is left as it is, so its handler and hooks
// still see what the tool's opt-in gives them. Only the calls that reach it
// move params to the heap.
func decodedArguments(params mcp.CallToolParams) any {
	return argsdecode.Decoded(&params)
}

// settleToolCall gives request, the dispatcher's own copy of a call, the
// Arguments argumentsAtOnce gave the handler's, so that the hooks after the
// call see what the handler saw. A call to a tool that opted in is left as
// it is.
func (s *MCPServer) settleToolCall(request *mcp.CallToolRequest) {
	if s.mayLeaveArgumentsToTheGetters(&request.Params) {
		argsdecode.Settle(&request.Params)
	}
}

// toolReadsArgumentsOnDemand reports whether the tool called name is
// registered with ServerTool.ArgumentsOnDemand.
func (s *MCPServer) toolReadsArgumentsOnDemand(name string) bool {
	s.toolsMu.RLock()
	tool := s.tools[name]
	onDemand := tool != nil && tool.ArgumentsOnDemand
	s.toolsMu.RUnlock()
	return onDemand
}

// noteArgumentsOnDemand turns on the per-call check of
// toolReadsArgumentsOnDemand once a tool in tools reads its arguments on
// demand. Called with toolsMu held.
func (s *MCPServer) noteArgumentsOnDemand(tools []ServerTool) {
	if s.argumentsOnDemand.Load() {
		return
	}
	for _, tool := range tools {
		if tool.ArgumentsOnDemand {
			s.readsArgumentsOnDemand = s.toolReadsArgumentsOnDemand
			s.argumentsOnDemand.Store(true)
			return
		}
	}
}

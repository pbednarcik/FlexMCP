package server

import (
	"encoding/json"

	"github.com/mark3labs/mcp-go/internal/argsdecode"
	"github.com/mark3labs/mcp-go/mcp"
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
// carries, for a call whose decode left them to the getters but whose tool,
// found when the call reaches it, does not read them on demand: one
// registered again without ServerTool.ArgumentsOnDemand since the call was
// decoded. Only the calls that reach it move params to the heap.
func argumentsAtOnce(params mcp.CallToolParams) mcp.CallToolParams {
	argsdecode.AtOnce(&params)
	return params
}

// toolReadsArgumentsOnDemand reports whether the tool called name is
// registered with ServerTool.ArgumentsOnDemand.
func (s *MCPServer) toolReadsArgumentsOnDemand(name string) bool {
	s.toolsMu.RLock()
	onDemand := s.tools[name].ArgumentsOnDemand
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

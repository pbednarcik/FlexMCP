package server

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/pbednarcik/flexmcp/mcp"
)

// AddPrompts registers multiple prompts at once
func (s *MCPServer) AddPrompts(prompts ...ServerPrompt) {
	s.implicitlyRegisterPromptCapabilities()

	s.promptsMu.Lock()
	s.addPromptsLocked(prompts)
	s.promptsMu.Unlock()

	s.notifyPromptsListChanged()
}

// addPromptsLocked is AddPrompts under promptsMu.
func (s *MCPServer) addPromptsLocked(prompts []ServerPrompt) {
	for _, entry := range prompts {
		s.prompts[entry.Prompt.Name] = entry.Prompt
		s.promptHandlers[entry.Prompt.Name] = entry.Handler
	}
}

// notifyPromptsListChanged tells every initialized session the prompts
// changed, when the server declared the listChanged capability.
func (s *MCPServer) notifyPromptsListChanged() {
	if s.capabilities.prompts.listChanged {
		s.SendNotificationToAllClients(mcp.MethodNotificationPromptsListChanged, nil)
	}
}

// AddPrompt registers a new prompt handler with the given name
func (s *MCPServer) AddPrompt(prompt mcp.Prompt, handler PromptHandlerFunc) {
	s.AddPrompts(ServerPrompt{Prompt: prompt, Handler: handler})
}

// SetPrompts replaces all existing prompts with the provided list, in one
// step: no read finds the prompts gone between the old and the new.
func (s *MCPServer) SetPrompts(prompts ...ServerPrompt) {
	s.implicitlyRegisterPromptCapabilities()

	s.promptsMu.Lock()
	s.prompts = make(map[string]mcp.Prompt, len(prompts))
	s.promptHandlers = make(map[string]PromptHandlerFunc, len(prompts))
	s.addPromptsLocked(prompts)
	s.promptsMu.Unlock()

	s.notifyPromptsListChanged()
}

// DeletePrompts removes prompts from the server
func (s *MCPServer) DeletePrompts(names ...string) {
	s.promptsMu.Lock()
	var exists bool
	for _, name := range names {
		if _, ok := s.prompts[name]; ok {
			delete(s.prompts, name)
			delete(s.promptHandlers, name)
			exists = true
		}
	}
	s.promptsMu.Unlock()

	// Send notification to all initialized sessions if listChanged capability is enabled, and we actually remove a prompt
	if exists && s.capabilities.prompts != nil && s.capabilities.prompts.listChanged {
		// Send notification to all initialized sessions
		s.SendNotificationToAllClients(mcp.MethodNotificationPromptsListChanged, nil)
	}
}

// ListPrompts returns a copy of the registered prompts map.
func (s *MCPServer) ListPrompts() map[string]*ServerPrompt {
	s.promptsMu.RLock()
	defer s.promptsMu.RUnlock()
	if len(s.prompts) == 0 {
		return nil
	}
	promptsCopy := make(map[string]*ServerPrompt, len(s.prompts))
	for name, prompt := range s.prompts {
		promptsCopy[name] = &ServerPrompt{
			Prompt:  prompt,
			Handler: s.promptHandlers[name],
		}
	}
	return promptsCopy
}

// filteredPrompts builds the full prompt candidate set and applies all
// registered prompt filters. This is the single source of truth for which
// prompts are visible in a given context, used by both handleListPrompts
// and handleGetPrompt to guarantee consistent behavior.
func (s *MCPServer) filteredPrompts(ctx context.Context) []mcp.Prompt {
	s.promptsMu.RLock()
	prompts := make([]mcp.Prompt, 0, len(s.prompts))
	for _, prompt := range s.prompts {
		prompts = append(prompts, prompt)
	}
	s.promptsMu.RUnlock()

	// sort prompts by name
	sort.Slice(prompts, func(i, j int) bool {
		return prompts[i].Name < prompts[j].Name
	})

	// Apply prompt filters if any are defined, outside the lock (see
	// toolFilterSnapshot).
	for _, filter := range s.promptFilterSnapshot() {
		prompts = filter(ctx, prompts)
	}

	return prompts
}

func (s *MCPServer) passesPromptFilters(ctx context.Context, prompt mcp.Prompt) bool {
	filters := s.promptFilterSnapshot()
	if len(filters) == 0 {
		return true
	}

	prompts := []mcp.Prompt{prompt}
	for _, filter := range filters {
		prompts = filter(ctx, prompts)
		if len(prompts) == 0 {
			return false
		}
	}

	for _, candidate := range prompts {
		if candidate.Name == prompt.Name {
			return true
		}
	}
	return false
}

// promptFilterSnapshot returns the prompt filters as they are now, to run
// outside the lock as toolFilterSnapshot describes.
func (s *MCPServer) promptFilterSnapshot() []PromptFilterFunc {
	s.promptFiltersMu.RLock()
	filters := s.promptFilters
	s.promptFiltersMu.RUnlock()
	return filters
}

func (s *MCPServer) handleListPrompts(
	ctx context.Context,
	id any,
	request mcp.ListPromptsRequest,
) (*mcp.ListPromptsResult, *requestError) {
	prompts := s.filteredPrompts(ctx)

	promptsToReturn, nextCursor, err := listByPagination(
		ctx,
		s,
		request.Params.Cursor,
		prompts,
	)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}
	result := mcp.ListPromptsResult{
		Prompts:    promptsToReturn,
		NextCursor: nextCursor,
	}
	return &result, nil
}

func (s *MCPServer) handleGetPrompt(
	ctx context.Context,
	id any,
	request mcp.GetPromptRequest,
) (*mcp.GetPromptResult, *requestError) {
	s.promptsMu.RLock()
	handler, ok := s.promptHandlers[request.Params.Name]
	prompt := s.prompts[request.Params.Name]
	s.promptsMu.RUnlock()

	if !ok {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  fmt.Errorf("prompt '%s' not found: %w", request.Params.Name, ErrPromptNotFound),
		}
	}

	// Enforce prompt filters at get time to prevent access to filtered-out
	// prompts. Only the requested prompt is passed through the filter chain so
	// this access check does not rebuild and filter the full prompt list.
	if !s.passesPromptFilters(ctx, prompt) {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  fmt.Errorf("prompt '%s' not found: %w", request.Params.Name, ErrPromptNotFound),
		}
	}

	// An argument the prompt declared required must be present (Prompts.md
	// row 10); the handler never sees a request that lacks one.
	for _, argument := range prompt.Arguments {
		if _, ok := request.Params.Arguments[argument.Name]; argument.Required && !ok {
			return nil, &requestError{
				id:   id,
				code: mcp.INVALID_PARAMS,
				err:  fmt.Errorf("prompt %q: missing required argument %q", request.Params.Name, argument.Name),
			}
		}
	}

	// The handler runs inside the middlewares, outermost first, the chain
	// built outside the lock (see toolFilterSnapshot).
	finalHandler := handler
	s.promptMiddlewareMu.RLock()
	mw := s.promptHandlerMiddlewares
	s.promptMiddlewareMu.RUnlock()
	for _, m := range slices.Backward(mw) {
		finalHandler = m(finalHandler)
	}

	result, err := finalHandler(ctx, request)
	if err != nil {
		return nil, handlerError(ctx, id, err)
	}

	// A handler may ask the client for more input (SEP-2322): a client on
	// protocol version 2026-07-28 or later receives the request and retries;
	// an older client cannot, and gets an error.
	result, err = resolveMultiRoundTrip(ctx, result, getPromptResultNeedsInput)
	if err != nil {
		return nil, handlerError(ctx, id, err)
	}

	// messages is a required array, so a prompt with no messages still sends
	// [] rather than null. A result asking for input has no messages yet. The
	// handler's result is copied rather than modified, as it may be shared.
	if result != nil && result.Messages == nil && !result.NeedsInput() {
		withMessages := *result
		withMessages.Messages = []mcp.PromptMessage{}
		result = &withMessages
	}

	// A handler that returns neither a result nor an error, on the first call
	// or on a retry, must still produce a response. The dispatcher would
	// otherwise dereference the nil result and panic.
	if result == nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INTERNAL_ERROR,
			err:  noResultError("prompt", request.Params.Name),
		}
	}

	return result, nil
}

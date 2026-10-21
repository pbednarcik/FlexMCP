package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mark3labs/mcp-go/mcp"
)

// applyStrictInputSchemaDefault fills in additionalProperties:false on a
// registered tool's structured input schema when WithStrictInputSchemaDefault
// is set and the author has not configured the field. Tools that ship a
// RawInputSchema are skipped — those bypass the structured-schema helpers
// and own additionalProperties themselves.
func (s *MCPServer) applyStrictInputSchemaDefault(tool *mcp.Tool) {
	if !s.strictInputSchemaDefault {
		return
	}
	if len(tool.RawInputSchema) > 0 {
		return
	}
	if tool.InputSchema.AdditionalProperties != nil {
		return
	}
	tool.InputSchema.AdditionalProperties = false
}

// validateToolHeaderAnnotations rejects a tool definition whose x-mcp-header
// annotations violate the SEP-2243 constraints, rather than emitting headers
// that gateways cannot route on. Servers MUST refuse such definitions.
func validateToolHeaderAnnotations(tool *mcp.Tool) error {
	if err := mcp.ValidateParamHeaderAnnotations(tool); err != nil {
		return fmt.Errorf("tool %q has invalid x-mcp-header annotations: %w", tool.Name, err)
	}
	return nil
}

// validateToolName rejects a tool name outside the form the specification
// asks for (Tools.md row 7): 1 to 128 characters of ASCII letters, digits,
// '_', '-' and '.'. Clients are told to refuse or rewrite other names, so a
// server refuses them at registration instead of advertising them.
func validateToolName(name string) error {
	valid := len(name) >= 1 && len(name) <= 128
	for i := 0; valid && i < len(name); i++ {
		c := name[i]
		valid = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
	}
	if !valid {
		return fmt.Errorf("tool name %q is not 1 to 128 characters of ASCII letters, digits, '_', '-' and '.'", name)
	}
	return nil
}

// checkToolDefinition runs every registration-time check on a tool: its name,
// its x-mcp-header annotations, and, with validation on, that its schemas
// compile. It returns the compiled schemas, uncached, for commitSchemas once
// the tool's batch is registered. Called with toolsMu held; the caller panics
// on an error, as a definition the server cannot serve is a programming error.
func (s *MCPServer) checkToolDefinition(tool *mcp.Tool) (toolSchemas, error) {
	if err := validateToolName(tool.Name); err != nil {
		return toolSchemas{}, err
	}
	if err := validateToolHeaderAnnotations(tool); err != nil {
		return toolSchemas{}, err
	}
	input, err := s.inputValidator.compileAtRegistration(*tool)
	if err != nil {
		return toolSchemas{}, err
	}
	output, err := s.outputValidator.compileAtRegistration(*tool)
	if err != nil {
		return toolSchemas{}, err
	}
	return toolSchemas{input: input, output: output}, nil
}

// toolSchemas are a tool's schemas as registration compiled them: nil for a
// schema the tool has none of, or with that validation off.
type toolSchemas struct {
	input, output *cachedToolSchema
}

// commitSchemas caches the schemas registration compiled for the tool called
// name, now registered, dropping what was cached for a schema it no longer
// has, and keeps its schemas with it. Called with toolsMu held.
func (s *MCPServer) commitSchemas(name string, schemas toolSchemas) {
	s.inputValidator.keep(name, schemas.input)
	s.outputValidator.keep(name, schemas.output)
	s.retainInputSchema(name, schemas.input.schema())
	s.retainOutputSchema(name, schemas.output.schema())
}

// retainOutputSchema keeps the output schema registration compiled for the
// tool called name, nil for a tool with none, so a call validates its result
// against it without reading the tool's schema again. Nothing is kept with
// output validation off. Called with toolsMu held.
func (s *MCPServer) retainOutputSchema(name string, compiled *jsonschema.Schema) {
	if s.outputValidator == nil {
		return
	}
	s.toolOutputSchemas[name] = compiled
}

// outputSchemaOf returns the output schema registration kept for the tool
// called name, and whether one was kept. Called with toolsMu held, so the
// schema belongs to the tool definition read under the same lock.
func (s *MCPServer) outputSchemaOf(name string) (*jsonschema.Schema, bool) {
	if s.outputValidator == nil {
		return nil, false
	}
	compiled, ok := s.toolOutputSchemas[name]
	return compiled, ok
}

// retainInputSchema keeps the input schema registration compiled for the tool
// called name, nil for a tool with none, so a call validates against it
// without reading the tool's schema again. Nothing is kept with input
// validation off. Called with toolsMu held.
func (s *MCPServer) retainInputSchema(name string, compiled *jsonschema.Schema) {
	if s.inputValidator == nil {
		return
	}
	s.toolInputSchemas[name] = compiled
}

// inputSchemaOf returns the input schema registration kept for the tool
// called name, and whether one was kept. Called with toolsMu held, so the
// schema belongs to the tool definition read under the same lock.
func (s *MCPServer) inputSchemaOf(name string) (*jsonschema.Schema, bool) {
	if s.inputValidator == nil {
		return nil, false
	}
	compiled, ok := s.toolInputSchemas[name]
	return compiled, ok
}

// AddTool registers a new tool and its handler. With input validation on,
// the input schema is compiled when the tool is registered: a schema changed
// after that is not seen by validation until the tool is registered again.
// Changing a registered tool's definition while calls run is a data race
// anyway.
func (s *MCPServer) AddTool(tool mcp.Tool, handler ToolHandlerFunc) {
	s.AddTools(ServerTool{Tool: tool, Handler: handler})
}

// AddTaskTool registers a new task tool and its handler. With input
// validation on, the input schema is compiled when the tool is registered: a
// schema changed after that is not seen by validation until the tool is
// registered again. Changing a registered tool's definition while calls run
// is a data race anyway.
func (s *MCPServer) AddTaskTool(tool mcp.Tool, handler TaskToolHandlerFunc) {
	s.AddTaskTools(ServerTaskTool{Tool: tool, Handler: handler})
}

// AddTools registers multiple tools at once. The batch is checked, and its
// schemas compiled, under the tools lock, so a rejected entry leaves the
// registry untouched; the cost falls on registration alone.
//
// With input validation on, each input schema is compiled when its tool is
// registered: a schema changed after that is not seen by validation until the
// tool is registered again. Changing a registered tool's definition while
// calls run is a data race anyway.
func (s *MCPServer) AddTools(tools ...ServerTool) {
	s.implicitlyRegisterToolCapabilities()

	s.toolsMu.Lock()
	// Stage the whole batch before touching the registry so a rejected entry
	// leaves earlier ones unregistered as well.
	staged := make(map[string]ServerTool, len(tools))
	schemas := make(map[string]toolSchemas, len(tools))
	for _, entry := range tools {
		name := entry.Tool.Name
		// Check for collision with task tools
		if _, exists := s.taskTools[name]; exists {
			s.toolsMu.Unlock()
			panic(fmt.Sprintf("tool name '%s' already registered as task tool", name))
		}
		s.applyStrictInputSchemaDefault(&entry.Tool)
		compiled, err := s.checkToolDefinition(&entry.Tool)
		if err != nil {
			s.toolsMu.Unlock()
			panic(err.Error())
		}
		staged[name] = entry
		schemas[name] = compiled
	}
	maps.Copy(s.tools, staged)
	for name, entry := range staged {
		s.toolHeaderBindings[name] = keepHeaderBindings(&entry.Tool)
		s.commitSchemas(name, schemas[name])
	}
	s.toolsMu.Unlock()

	// When the list of available tools changes, servers that declared the listChanged capability SHOULD send a notification.
	if s.capabilities.tools.listChanged {
		// Send notification to all initialized sessions
		s.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
	}
}

// AddTaskTools registers multiple task tools at once. With input validation
// on, each input schema is compiled when its tool is registered: a schema
// changed after that is not seen by validation until the tool is registered
// again. Changing a registered tool's definition while calls run is a data
// race anyway.
func (s *MCPServer) AddTaskTools(taskTools ...ServerTaskTool) {
	s.implicitlyRegisterToolCapabilities()

	s.toolsMu.Lock()
	staged := make(map[string]ServerTaskTool, len(taskTools))
	schemas := make(map[string]toolSchemas, len(taskTools))
	for _, entry := range taskTools {
		name := entry.Tool.Name
		// Check for collision with regular tools
		if _, exists := s.tools[name]; exists {
			s.toolsMu.Unlock()
			panic(fmt.Sprintf("task tool name '%s' already registered as regular tool", name))
		}
		s.applyStrictInputSchemaDefault(&entry.Tool)
		compiled, err := s.checkToolDefinition(&entry.Tool)
		if err != nil {
			s.toolsMu.Unlock()
			panic(err.Error())
		}
		staged[name] = entry
		schemas[name] = compiled
	}
	maps.Copy(s.taskTools, staged)
	for name, entry := range staged {
		s.toolHeaderBindings[name] = keepHeaderBindings(&entry.Tool)
		s.commitSchemas(name, schemas[name])
	}
	s.toolsMu.Unlock()

	// When the list of available tools changes, servers that declared the listChanged capability SHOULD send a notification.
	if s.capabilities.tools.listChanged {
		// Send notification to all initialized sessions
		s.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
	}
}

// SetTools replaces all existing tools with the provided list. With input
// validation on, each input schema is compiled when its tool is registered: a
// schema changed after that is not seen by validation until the tool is
// registered again. Changing a registered tool's definition while calls run
// is a data race anyway.
func (s *MCPServer) SetTools(tools ...ServerTool) {
	s.implicitlyRegisterToolCapabilities()

	s.toolsMu.Lock()
	newTools := make(map[string]ServerTool, len(tools))
	schemas := make(map[string]toolSchemas, len(tools))
	for _, entry := range tools {
		name := entry.Tool.Name
		// Check for collision with task tools
		if _, exists := s.taskTools[name]; exists {
			s.toolsMu.Unlock()
			panic(fmt.Sprintf("tool name '%s' already registered as task tool", name))
		}
		s.applyStrictInputSchemaDefault(&entry.Tool)
		compiled, err := s.checkToolDefinition(&entry.Tool)
		if err != nil {
			s.toolsMu.Unlock()
			panic(err.Error())
		}
		newTools[name] = entry
		schemas[name] = compiled
	}
	// The old tools' cached schemas go first, so the new ones compiled above
	// are the ones kept.
	for name := range s.tools {
		delete(s.toolHeaderBindings, name)
		delete(s.toolInputSchemas, name)
		delete(s.toolOutputSchemas, name)
		s.inputValidator.invalidate(name)
		s.outputValidator.invalidate(name)
	}
	s.tools = newTools
	for name, entry := range newTools {
		s.toolHeaderBindings[name] = keepHeaderBindings(&entry.Tool)
		s.commitSchemas(name, schemas[name])
	}
	s.toolsMu.Unlock()

	// When the list of available tools changes, servers that declared the listChanged capability SHOULD send a notification.
	if s.capabilities.tools.listChanged {
		// Send notification to all initialized sessions
		s.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
	}
}

// GetTool retrieves the specified tool
func (s *MCPServer) GetTool(toolName string) *ServerTool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	if tool, ok := s.tools[toolName]; ok {
		return &tool
	}
	return nil
}

func (s *MCPServer) ListTools() map[string]*ServerTool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	if len(s.tools) == 0 {
		return nil
	}
	// Create a copy to prevent external modification
	toolsCopy := make(map[string]*ServerTool, len(s.tools))
	for name, tool := range s.tools {
		toolsCopy[name] = &ServerTool{
			Tool:    tool.Tool,
			Handler: tool.Handler,
		}
	}
	return toolsCopy
}

// DeleteTools removes tools from the server
func (s *MCPServer) DeleteTools(names ...string) {
	s.toolsMu.Lock()
	var exists bool
	for _, name := range names {
		if _, ok := s.tools[name]; ok {
			delete(s.tools, name)
			delete(s.toolHeaderBindings, name)
			delete(s.toolInputSchemas, name)
			delete(s.toolOutputSchemas, name)
			exists = true
		}
	}
	s.toolsMu.Unlock()

	// Drop any cached compiled input/output schemas for the removed tools so
	// a tool re-added later under the same name does not reuse a stale
	// compilation.
	s.inputValidator.invalidate(names...)
	s.outputValidator.invalidate(names...)

	// When the list of available tools changes, servers that declared the listChanged capability SHOULD send a notification.
	if exists && s.capabilities.tools != nil && s.capabilities.tools.listChanged {
		// Send notification to all initialized sessions
		s.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
	}
}

// filteredTools builds the full tool candidate set (global + task) and
// applies all registered tool filters. This is the single source of truth
// for which tools are visible in a given context, used by both handleListTools
// and handleToolCall to guarantee consistent behavior.
func (s *MCPServer) filteredTools(ctx context.Context) []mcp.Tool {
	// Get the base tools from the server (both regular and task tools)
	s.toolsMu.RLock()
	tools := make([]mcp.Tool, 0, len(s.tools)+len(s.taskTools))

	// Get all tool names for consistent ordering
	toolNames := make([]string, 0, len(s.tools)+len(s.taskTools))
	for name := range s.tools {
		toolNames = append(toolNames, name)
	}
	for name := range s.taskTools {
		toolNames = append(toolNames, name)
	}

	// Sort the tool names for consistent ordering
	sort.Strings(toolNames)

	// Add tools in sorted order
	for _, name := range toolNames {
		if tool, ok := s.tools[name]; ok {
			tools = append(tools, tool.Tool)
		} else if taskTool, ok := s.taskTools[name]; ok {
			tools = append(tools, taskTool.Tool)
		}
	}
	s.toolsMu.RUnlock()

	// Apply tool filters if any are defined
	s.toolFiltersMu.RLock()
	if len(s.toolFilters) > 0 {
		for _, filter := range s.toolFilters {
			tools = filter(ctx, tools)
		}
	}
	s.toolFiltersMu.RUnlock()

	return tools
}

func (s *MCPServer) passesToolFilters(ctx context.Context, tool mcp.Tool) bool {
	s.toolFiltersMu.RLock()
	defer s.toolFiltersMu.RUnlock()
	if len(s.toolFilters) == 0 {
		return true
	}

	tools := []mcp.Tool{tool}
	for _, filter := range s.toolFilters {
		tools = filter(ctx, tools)
		if len(tools) == 0 {
			return false
		}
	}

	for _, candidate := range tools {
		if candidate.Name == tool.Name {
			return true
		}
	}
	return false
}

func (s *MCPServer) handleListTools(
	ctx context.Context,
	id any,
	request mcp.ListToolsRequest,
) (*mcp.ListToolsResult, *requestError) {
	tools := s.filteredTools(ctx)

	// Apply pagination
	toolsToReturn, nextCursor, err := listByPagination(
		ctx,
		s,
		request.Params.Cursor,
		tools,
	)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	result := mcp.ListToolsResult{
		Tools:      toolsToReturn,
		NextCursor: nextCursor,
	}
	return &result, nil
}

func (s *MCPServer) handleToolCall(
	ctx context.Context,
	id any,
	request mcp.CallToolRequest,
) (any, *requestError) {
	// Find the tool: the server's own first, then the task tools.
	var taskToolOnly bool
	s.toolsMu.RLock()
	tool, ok := s.tools[request.Params.Name]
	if !ok {
		if taskTool, taskOk := s.taskTools[request.Params.Name]; taskOk {
			// Convert ServerTaskTool to ServerTool for validation
			// The tool metadata is the same, we just need it for checking task support
			tool = ServerTool{
				Tool:    taskTool.Tool,
				Handler: nil, // Handler will be used from taskTool in handleTaskAugmentedToolCall
			}
			ok = true
			taskToolOnly = true
		}
	}
	inputSchema, inputSchemaKept := s.inputSchemaOf(request.Params.Name)
	outputSchema, outputSchemaKept := s.outputSchemaOf(request.Params.Name)
	s.toolsMu.RUnlock()

	if !ok {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  fmt.Errorf("tool '%s' not found: %w", request.Params.Name, ErrToolNotFound),
		}
	}

	// Enforce tool filters at call time to prevent access to filtered-out
	// tools. Only the requested tool is passed through the filter chain so this
	// access check does not rebuild and filter the full tool list.
	if !s.passesToolFilters(ctx, tool.Tool) {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  fmt.Errorf("tool '%s' not found: %w", request.Params.Name, ErrToolNotFound),
		}
	}

	// Validate task support requirements
	if tool.Tool.Execution != nil && tool.Tool.Execution.TaskSupport == mcp.TaskSupportRequired {
		if request.Params.Task == nil {
			return nil, &requestError{
				id:   id,
				code: mcp.METHOD_NOT_FOUND,
				err:  fmt.Errorf("tool '%s' requires task augmentation", request.Params.Name),
			}
		}
	}

	// Check if this should be executed as a task (hybrid mode support)
	// Tools with TaskSupportOptional or TaskSupportRequired can be executed as tasks
	shouldExecuteAsTask := request.Params.Task != nil &&
		tool.Tool.Execution != nil &&
		(tool.Tool.Execution.TaskSupport == mcp.TaskSupportOptional ||
			tool.Tool.Execution.TaskSupport == mcp.TaskSupportRequired)

	// A task-augmented call reaches a tool whose execution allows tasks, on
	// a server that serves them; any other is refused as method not found
	// (2025-11-25 Tasks.md row 28), never run as a synchronous call instead.
	if request.Params.Task != nil {
		if !shouldExecuteAsTask {
			return nil, &requestError{
				id:   id,
				code: mcp.METHOD_NOT_FOUND,
				err:  fmt.Errorf("tool '%s' does not support task augmentation", request.Params.Name),
			}
		}
		if !s.servesTasks() {
			return nil, &requestError{
				id:   id,
				code: mcp.METHOD_NOT_FOUND,
				err:  errors.New("this server does not serve tasks"),
			}
		}
		return s.handleTaskAugmentedToolCall(ctx, id, request)
	}

	if taskToolOnly {
		return nil, &requestError{
			id:   id,
			code: mcp.METHOD_NOT_FOUND,
			err:  fmt.Errorf("tool '%s' does not support synchronous execution", request.Params.Name),
		}
	}

	// Validate the incoming arguments against the tool's input schema, when
	// schema validation has been enabled via WithInputSchemaValidation. A
	// validation failure is returned as a SEP-1303 tool execution error so the
	// model receives feedback in its context window and can self-correct.
	if s.inputValidator != nil {
		if _, err := s.inputValidator.validateRegistered(tool.Tool, inputSchema, inputSchemaKept, s.argumentsToValidate(request)); err != nil {
			return validationToolResult(err), nil
		}
	}

	// The handler runs inside the middlewares, outermost first. The chain is
	// built outside the lock, from the slice as it is now: Use only appends,
	// so what the slice holds does not change under a reader, and a
	// middleware that panics while wrapping does not keep the lock.
	finalHandler := tool.Handler
	s.toolMiddlewareMu.RLock()
	mw := s.toolHandlerMiddlewares
	s.toolMiddlewareMu.RUnlock()
	for _, m := range slices.Backward(mw) {
		finalHandler = m(finalHandler)
	}

	result, err := finalHandler(ctx, request)
	if err != nil {
		return nil, handlerError(ctx, id, err)
	}

	// A handler may ask the client for more input before it can finish
	// (SEP-2322): a client on protocol version 2026-07-28 or later receives
	// the request and retries; an older client cannot, and gets an error.
	result, err = resolveMultiRoundTrip(ctx, result, callToolResultNeedsInput)
	if err != nil {
		return nil, handlerError(ctx, id, err)
	}

	// Without this check a handler that returns neither a result nor an
	// error, on the first call or on a retry, would be answered with a null
	// result, which no client accepts.
	if result == nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INTERNAL_ERROR,
			err:  noResultError("tool", request.Params.Name),
		}
	}

	// Validate the tool's StructuredContent against its declared output
	// schema when output schema validation has been enabled via
	// WithOutputSchemaValidation. A validation failure is surfaced as a
	// tool execution error so the client cannot silently receive a result
	// that violates the declared contract.
	if s.outputValidator != nil {
		if _, vErr := s.outputValidator.validate(tool.Tool, outputSchema, outputSchemaKept, result); vErr != nil {
			return validationToolResult(vErr), nil
		}
	}

	return s.limitToolResult(result), nil
}

// limitToolResult returns result when it fits in the configured limit.
// An oversized payload is replaced with an error result so those bytes are
// not sent or stored. CreateTaskResult hides Content behind json:"-", so
// callers that store one must measure a CallToolResult view of the same fields.
func (s *MCPServer) limitToolResult(result *mcp.CallToolResult) *mcp.CallToolResult {
	if result == nil || s.toolResultSizeLimit <= 0 {
		return result
	}
	// The encoding is counted, not kept; Encode adds a newline Marshal does not.
	var size byteCounter
	if err := json.NewEncoder(&size).Encode(result); err != nil || int(size)-1 <= s.toolResultSizeLimit {
		return result
	}
	return mcp.NewToolResultError(fmt.Sprintf("tool result exceeds %d bytes", s.toolResultSizeLimit))
}

// byteCounter counts the bytes written to it and keeps none.
type byteCounter int

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}

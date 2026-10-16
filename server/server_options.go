package server

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// WithCacheHints sets the default SEP-2549 caching hints advertised on
// tools/list, prompts/list, resources/list, resources/templates/list,
// resources/read, and server/discover results.
//
// ttlMs is a freshness hint in milliseconds: clients may reuse a cached
// response for that long before re-fetching. Zero, the default, asks clients
// to revalidate every time. scope controls whether shared intermediaries may
// cache the response across authorization contexts.
//
// The hints are only emitted to clients using protocol version 2026-07-28 or
// later, which is where the fields were introduced. A negative ttlMs is not
// a value the protocol allows and is advertised as 0.
func WithCacheHints(ttlMs int64, scope mcp.CacheScope) ServerOption {
	return func(s *MCPServer) {
		s.capabilitiesMu.Lock()
		defer s.capabilitiesMu.Unlock()
		if s.cacheHints == nil {
			s.cacheHints = make(map[mcp.MCPMethod]cacheHints)
		}
		s.cacheHints[""] = cacheHints{ttlMs: max(ttlMs, 0), scope: scope}
	}
}

// WithMethodCacheHints sets the SEP-2549 caching hints advertised on the
// result of a single method, overriding any default set by [WithCacheHints].
// A negative ttlMs is advertised as 0.
func WithMethodCacheHints(method mcp.MCPMethod, ttlMs int64, scope mcp.CacheScope) ServerOption {
	return func(s *MCPServer) {
		s.capabilitiesMu.Lock()
		defer s.capabilitiesMu.Unlock()
		if s.cacheHints == nil {
			s.cacheHints = make(map[mcp.MCPMethod]cacheHints)
		}
		s.cacheHints[method] = cacheHints{ttlMs: max(ttlMs, 0), scope: scope}
	}
}

// WithPaginationLimit sets the maximum page size for list results.
// Zero and negative limits leave the list unpaged.
func WithPaginationLimit(limit int) ServerOption {
	return func(s *MCPServer) {
		s.paginationLimit = &limit
	}
}

// WithToolResultSizeLimit replaces a tool result whose JSON encoding is
// larger than maxBytes with an error result. The oversized payload is not
// sent. The same check runs for a synchronous call and for both task
// execution paths, before the result is stored. Zero and negative limits
// disable the check.
func WithToolResultSizeLimit(maxBytes int) ServerOption {
	return func(s *MCPServer) {
		s.toolResultSizeLimit = maxBytes
	}
}

// WithResourceCapabilities configures resource-related server capabilities:
// listChanged announces resource list changes to clients that listen for
// them. subscribe is accepted and ignored: this server emits no
// notifications/resources/updated yet, so it advertises no per-resource
// subscriptions and grants none on subscriptions/listen, whatever is asked.
func WithResourceCapabilities(_ bool, listChanged bool) ServerOption {
	return func(s *MCPServer) {
		// Always create a non-nil capability object
		s.capabilities.resources = &resourceCapabilities{
			listChanged: listChanged,
		}
		s.markServed(servesResources)
	}
}

// WithPromptCompletionProvider sets a custom prompt completion provider
func WithPromptCompletionProvider(provider PromptCompletionProvider) ServerOption {
	return func(s *MCPServer) {
		s.promptCompletionProvider = provider
	}
}

// WithResourceCompletionProvider sets a custom resource completion provider
func WithResourceCompletionProvider(provider ResourceCompletionProvider) ServerOption {
	return func(s *MCPServer) {
		s.resourceCompletionProvider = provider
	}
}

// WithToolHandlerMiddleware allows adding a middleware for the
// tool handler call chain.
func WithToolHandlerMiddleware(
	toolHandlerMiddleware ToolHandlerMiddleware,
) ServerOption {
	return func(s *MCPServer) {
		s.toolMiddlewareMu.Lock()
		s.toolHandlerMiddlewares = append(s.toolHandlerMiddlewares, toolHandlerMiddleware)
		s.toolMiddlewareMu.Unlock()
	}
}

// Use adds one or more tool handler middlewares to the server.
// Middleware is applied in the order added (outermost first), matching net/http convention.
// A task tool registered with AddTaskTools is not wrapped by these: it has
// a chain of its own, see UseForTaskTools.
func (s *MCPServer) Use(mw ...ToolHandlerMiddleware) {
	s.toolMiddlewareMu.Lock()
	s.toolHandlerMiddlewares = append(s.toolHandlerMiddlewares, mw...)
	s.toolMiddlewareMu.Unlock()
}

// WithTaskToolHandlerMiddleware adds a middleware to the task tool handler
// call chain; see UseForTaskTools.
func WithTaskToolHandlerMiddleware(mw TaskToolHandlerMiddleware) ServerOption {
	return func(s *MCPServer) {
		s.UseForTaskTools(mw)
	}
}

// UseForTaskTools adds one or more task tool handler middlewares to the
// server, applied in the order added, outermost first, around the handler of
// every task tool registered with AddTaskTools. A middleware that returns an
// error fails the task with it. The tool middlewares (Use) do not wrap task
// tools: their chain carries a CallToolResult, a task tool's handler returns
// task creation info.
func (s *MCPServer) UseForTaskTools(mw ...TaskToolHandlerMiddleware) {
	s.toolMiddlewareMu.Lock()
	s.taskToolHandlerMiddlewares = append(s.taskToolHandlerMiddlewares, mw...)
	s.toolMiddlewareMu.Unlock()
}

// WithResourceHandlerMiddleware allows adding a middleware for the
// resource handler call chain.
func WithResourceHandlerMiddleware(
	resourceHandlerMiddleware ResourceHandlerMiddleware,
) ServerOption {
	return func(s *MCPServer) {
		s.resourceMiddlewareMu.Lock()
		s.resourceHandlerMiddlewares = append(s.resourceHandlerMiddlewares, resourceHandlerMiddleware)
		s.resourceMiddlewareMu.Unlock()
	}
}

// WithResourceRecovery adds a middleware that recovers from panics in resource handlers.
func WithResourceRecovery() ServerOption {
	return WithResourceHandlerMiddleware(func(next ResourceHandlerFunc) ResourceHandlerFunc {
		return func(ctx context.Context, request mcp.ReadResourceRequest) (result []mcp.ResourceContents, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf(
						"panic recovered in %s resource handler: %v",
						request.Params.URI,
						r,
					)
				}
			}()
			return next(ctx, request)
		}
	})
}

// WithToolFilter adds a filter function that controls tool visibility and access.
// The filter is applied both when listing tools (tools/list) and when calling
// tools (tools/call). A tool that is filtered out cannot be discovered or
// invoked, ensuring that filters act as an access control boundary rather than
// just a visibility hint. Call-time checks pass only the requested tool to the
// filter, while list-time checks pass the full candidate list.
func WithToolFilter(
	toolFilter ToolFilterFunc,
) ServerOption {
	return func(s *MCPServer) {
		s.toolFiltersMu.Lock()
		s.toolFilters = append(s.toolFilters, toolFilter)
		s.toolFiltersMu.Unlock()
	}
}

// WithPromptHandlerMiddleware allows adding a middleware for the
// prompt handler call chain.
func WithPromptHandlerMiddleware(
	promptHandlerMiddleware PromptHandlerMiddleware,
) ServerOption {
	return func(s *MCPServer) {
		s.promptMiddlewareMu.Lock()
		s.promptHandlerMiddlewares = append(s.promptHandlerMiddlewares, promptHandlerMiddleware)
		s.promptMiddlewareMu.Unlock()
	}
}

// WithPromptFilter adds a filter function that controls prompt visibility and
// access. The filter is applied both when listing prompts (prompts/list) and
// when retrieving a prompt (prompts/get). A prompt that is filtered out cannot
// be discovered or accessed. Get-time checks pass only the requested prompt to
// the filter, while list-time checks pass the full candidate list.
func WithPromptFilter(
	promptFilter PromptFilterFunc,
) ServerOption {
	return func(s *MCPServer) {
		s.promptFiltersMu.Lock()
		s.promptFilters = append(s.promptFilters, promptFilter)
		s.promptFiltersMu.Unlock()
	}
}

// WithRecovery adds a middleware that recovers from panics in tool handlers.
func WithRecovery() ServerOption {
	return WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf(
						"panic recovered in %s tool handler: %v",
						request.Params.Name,
						r,
					)
				}
			}()
			return next(ctx, request)
		}
	})
}

// WithInputSchemaValidation enables server-side validation of tool call
// arguments against each tool's declared inputSchema. When validation fails
// the server returns a tool execution error result (CallToolResult with
// IsError: true) per [SEP-1303] so that the language model receives the
// failure details in its context window and can self-correct (for example,
// when it sends an unknown parameter name).
//
// Validation is opt-in to preserve backwards compatibility with servers whose
// hand-written schemas may not perfectly describe the arguments their handlers
// actually accept. Enabling it is recommended for new servers and for any
// server whose schemas are accurate.
//
// A tool whose input schema cannot be compiled (malformed JSON, an
// unsupported dialect, a $ref the compiler will not fetch) is refused at
// registration, which panics as it does for an invalid x-mcp-header
// annotation: the schema is compiled once there and kept, so no call pays
// for it, and no tool is served unvalidated by mistake.
//
// [SEP-1303]: https://modelcontextprotocol.io/seps/1303-input-validation-errors-as-tool-execution-errors
func WithInputSchemaValidation() ServerOption {
	return func(s *MCPServer) {
		if s.inputValidator == nil {
			s.inputValidator = newInputSchemaValidator()
		}
	}
}

// WithStrictInputSchemaDefault sets additionalProperties:false on every
// registered tool's input schema when the tool author has not configured the
// field explicitly. Tools published by such a server reject unknown property
// names — at the client (which sees the strict schema in tools/list) and at
// the server when [WithInputSchemaValidation] is also enabled.
//
// Tools that supply [mcp.Tool.RawInputSchema] are not modified: those authors
// have opted out of the structured-schema helpers and own additionalProperties
// themselves. Tools that explicitly call
// [mcp.WithSchemaAdditionalProperties] are left untouched, so a single tool
// can opt back into permissive behaviour while the server default stays
// strict.
//
// The option is independent of [WithInputSchemaValidation]: setting strict
// schemas without server-side enforcement still steers schema-aware clients
// and language models away from unknown arguments.
func WithStrictInputSchemaDefault() ServerOption {
	return func(s *MCPServer) {
		s.strictInputSchemaDefault = true
	}
}

// WithOutputSchemaValidation enables server-side validation of tool call
// results against each tool's declared outputSchema. When a tool returns a
// CallToolResult whose StructuredContent does not conform to the schema, the
// server replaces the result with a tool execution error (CallToolResult with
// IsError: true) so the client never sees a result that violates the
// declared contract.
//
// The MCP tools specification states that when an outputSchema is provided,
// the tool result MUST include structuredContent conforming to that schema.
// Enabling this option enforces that contract at runtime.
//
// Validation is skipped for results whose StructuredContent is nil and for
// error results (IsError: true). A tool whose output schema cannot be
// compiled is refused at registration, as WithInputSchemaValidation refuses
// an input schema.
//
// This option is opt-in to preserve backwards compatibility with servers
// whose hand-written output schemas may not perfectly describe the values
// their handlers actually return. Enabling it is recommended for any server
// whose schemas are accurate.
func WithOutputSchemaValidation() ServerOption {
	return func(s *MCPServer) {
		if s.outputValidator == nil {
			s.outputValidator = newOutputSchemaValidator()
		}
	}
}

// WithHooks allows adding hooks that will be called before or after
// either [all] requests or before / after specific request methods, or else
// prior to returning an error to the client.
func WithHooks(hooks *Hooks) ServerOption {
	return func(s *MCPServer) {
		s.hooks = hooks
	}
}

// GetHooks returns the server's current Hooks instance, or nil if no hooks
// have been configured. The returned pointer can be used to add additional
// hooks via the Add* methods without replacing existing hook registrations.
func (s *MCPServer) GetHooks() *Hooks {
	return s.hooks
}

// WithTaskHooks allows adding hooks for task lifecycle events.
// Use these hooks to monitor task execution, track metrics, and observe
// task-augmented tool behavior.
func WithTaskHooks(taskHooks *TaskHooks) ServerOption {
	return func(s *MCPServer) {
		s.taskHooks = taskHooks
	}
}

// WithMaxConcurrentTasks sets a limit on the maximum number of concurrent running tasks.
// When this limit is reached, attempts to create new tasks will fail with an error.
// If not set (or set to 0), there is no limit on concurrent tasks.
func WithMaxConcurrentTasks(limit int) ServerOption {
	return func(s *MCPServer) {
		s.maxConcurrentTasks = &limit
	}
}

// WithTaskTTL bounds how long a task is kept after its creation: a request
// asking for a longer TTL, or none, gets this one, and the task reports the
// TTL it got. A task's result is held in memory until its TTL passes, and a
// client that polls rather than listens may never fetch it, so a server
// open to many clients wants a bound. Without the option a task is kept
// for as long as its request asks, and for good when it asks nothing, as
// the other SDKs' stores keep theirs; zero or less says the same. That
// includes a task that ended at once, its arguments failing validation or
// a hook of its creation panicking: it is kept like any other.
func WithTaskTTL(d time.Duration) ServerOption {
	return func(s *MCPServer) {
		s.taskTTL = d
	}
}

// WithPromptCapabilities configures prompt-related server capabilities
func WithPromptCapabilities(listChanged bool) ServerOption {
	return func(s *MCPServer) {
		// Always create a non-nil capability object
		s.capabilities.prompts = &promptCapabilities{
			listChanged: listChanged,
		}
		s.markServed(servesPrompts)
	}
}

// WithToolCapabilities configures tool-related server capabilities
func WithToolCapabilities(listChanged bool) ServerOption {
	return func(s *MCPServer) {
		// Always create a non-nil capability object
		s.capabilities.tools = &toolCapabilities{
			listChanged: listChanged,
		}
		s.markServed(servesTools)
	}
}

// WithLogging enables logging capabilities for the server
func WithLogging() ServerOption {
	return func(s *MCPServer) {
		s.capabilities.logging = new(true)
		s.markServed(servesLogging)
	}
}

// WithTaskCapabilities configures task-related server capabilities
func WithTaskCapabilities(list, cancel, toolCallTasks bool) ServerOption {
	return func(s *MCPServer) {
		// Always create a non-nil capability object
		s.capabilities.tasks = &taskCapabilities{
			list:          list,
			cancel:        cancel,
			toolCallTasks: toolCallTasks,
		}
		s.markServed(servesTasks)
	}
}

// WithInstructions sets the server instructions for the client returned in the initialize response
func WithInstructions(instructions string) ServerOption {
	return func(s *MCPServer) {
		s.instructions = instructions
	}
}

// WithCompletions enables the completion capability
func WithCompletions() ServerOption {
	return func(s *MCPServer) {
		s.capabilities.completions = new(true)
		s.markServed(servesCompletions)
	}
}

// WithExperimental sets experimental, non-standard capabilities on the server.
func WithExperimental(experimental map[string]any) ServerOption {
	return func(s *MCPServer) {
		s.capabilities.experimental = experimental
	}
}

// WithExtensions advertises support for optional MCP extensions beyond the
// core protocol, as a map of extension identifier to that extension's settings
// object. An empty object means the extension is supported with no additional
// settings.
//
// Extension identifiers follow the _meta key naming rules, so they carry a
// mandatory prefix - for example "io.modelcontextprotocol/tasks" for the Tasks
// extension, or "io.modelcontextprotocol/ui" for MCP Apps.
//
// Extensions were formalized in protocol version 2026-07-28. When one party
// supports an extension and the other does not, the supporting party must
// either fall back to core protocol behaviour or reject the request.
func WithExtensions(extensions map[string]any) ServerOption {
	return func(s *MCPServer) {
		s.capabilities.extensions = extensions
	}
}

// WithIcons sets the server icons for the implementation metadata returned
// during initialization. The icons slice and nested Sizes fields are defensively
// copied to prevent external mutation.
func WithIcons(icons ...mcp.Icon) ServerOption {
	return func(s *MCPServer) {
		copied := make([]mcp.Icon, len(icons))
		for i, icon := range icons {
			copied[i] = icon
			if icon.Sizes != nil {
				copied[i].Sizes = make([]string, len(icon.Sizes))
				copy(copied[i].Sizes, icon.Sizes)
			}
		}
		s.implementation.Icons = copied
	}
}

// WithTitle sets the human-readable display title for the server implementation.
func WithTitle(title string) ServerOption {
	return func(s *MCPServer) {
		s.implementation.Title = title
	}
}

// WithDescription sets the description for the server implementation.
func WithDescription(description string) ServerOption {
	return func(s *MCPServer) {
		s.implementation.Description = description
	}
}

// WithWebsiteURL sets the website URL for the server implementation.
func WithWebsiteURL(websiteURL string) ServerOption {
	return func(s *MCPServer) {
		s.implementation.WebsiteURL = websiteURL
	}
}

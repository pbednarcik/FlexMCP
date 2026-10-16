// Package server provides MCP (Model Context Protocol) server implementations.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/tracing"
)

// ServerOption is a function that configures an MCPServer.
type ServerOption func(*MCPServer)

// ResourceHandlerFunc is a function that returns resource contents.
type ResourceHandlerFunc func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error)

// ResourceTemplateHandlerFunc is a function that returns a resource template.
type ResourceTemplateHandlerFunc func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error)

// PromptHandlerFunc handles prompt requests with given arguments.
type PromptHandlerFunc func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error)

// ToolHandlerFunc handles tool calls with given arguments.
type ToolHandlerFunc func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error)

// TaskToolHandlerFunc handles tool calls that execute asynchronously.
// It returns immediately with task creation info; the actual result is
// retrieved later via tasks/result.
type TaskToolHandlerFunc func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CreateTaskResult, error)

// ToolHandlerMiddleware is a middleware function that wraps a ToolHandlerFunc.
type ToolHandlerMiddleware func(ToolHandlerFunc) ToolHandlerFunc

// TaskToolHandlerMiddleware wraps a TaskToolHandlerFunc, as
// ToolHandlerMiddleware wraps a ToolHandlerFunc. A task tool's handler
// returns task creation info, so the tool middlewares, whose chain carries
// a CallToolResult, cannot wrap it: the task tools have a chain of their
// own, see UseForTaskTools.
type TaskToolHandlerMiddleware func(TaskToolHandlerFunc) TaskToolHandlerFunc

// ResourceHandlerMiddleware is a middleware function that wraps a ResourceHandlerFunc.
type ResourceHandlerMiddleware func(ResourceHandlerFunc) ResourceHandlerFunc

// ToolFilterFunc is a function that filters tools based on context, typically
// using session information. Filters are applied both when listing tools
// (tools/list) and when calling tools (tools/call), so a filtered-out tool
// cannot be discovered or invoked. During tools/call, filters receive only the
// requested tool to keep the call-time access check off the full-list hot path.
type ToolFilterFunc func(ctx context.Context, tools []mcp.Tool) []mcp.Tool

// PromptHandlerMiddleware is a middleware function that wraps a PromptHandlerFunc.
type PromptHandlerMiddleware func(PromptHandlerFunc) PromptHandlerFunc

// PromptFilterFunc is a function that filters prompts based on context,
// typically using session information. Filters are applied both when listing
// prompts (prompts/list) and when retrieving prompts (prompts/get), so a
// filtered-out prompt cannot be discovered or accessed. During prompts/get,
// filters receive only the requested prompt to keep the get-time access check
// off the full-list hot path.
type PromptFilterFunc func(ctx context.Context, prompts []mcp.Prompt) []mcp.Prompt

// ServerTool combines a Tool with its ToolHandlerFunc.
type ServerTool struct {
	Tool    mcp.Tool
	Handler ToolHandlerFunc
}

// ServerTaskTool combines a Tool with its TaskToolHandlerFunc.
type ServerTaskTool struct {
	Tool    mcp.Tool
	Handler TaskToolHandlerFunc
}

// ServerPrompt combines a Prompt with its handler function.
type ServerPrompt struct {
	Prompt  mcp.Prompt
	Handler PromptHandlerFunc
}

// ServerResource combines a Resource with its handler function.
type ServerResource struct {
	Resource mcp.Resource
	Handler  ResourceHandlerFunc
}

// ServerResourceTemplate combines a ResourceTemplate with its handler function.
type ServerResourceTemplate struct {
	Template mcp.ResourceTemplate
	Handler  ResourceTemplateHandlerFunc
}

// serverKey is the context key for storing the server instance
type serverKey struct{}

// ServerFromContext retrieves the MCPServer instance from a context
func ServerFromContext(ctx context.Context) *MCPServer {
	if srv, ok := ctx.Value(serverKey{}).(*MCPServer); ok {
		return srv
	}
	return nil
}

// NotificationHandlerFunc handles incoming notifications.
type NotificationHandlerFunc func(ctx context.Context, notification mcp.JSONRPCNotification)

// MCPServer implements a Model Context Protocol server that can handle various types of requests
// including resources, prompts, and tools.
type MCPServer struct {
	// Separate mutexes for different resource types
	resourcesMu            sync.RWMutex
	resourceMiddlewareMu   sync.RWMutex
	promptsMu              sync.RWMutex
	toolsMu                sync.RWMutex
	toolMiddlewareMu       sync.RWMutex
	promptMiddlewareMu     sync.RWMutex
	notificationHandlersMu sync.RWMutex
	capabilitiesMu         sync.RWMutex
	toolFiltersMu          sync.RWMutex
	promptFiltersMu        sync.RWMutex
	tasksMu                sync.RWMutex

	name              string
	version           string
	implementation    mcp.Implementation
	serverInfo        any // serverImplementation boxed once, for result _meta
	instructions      string
	resources         map[string]resourceEntry
	resourceTemplates map[string]resourceTemplateEntry
	// resourceTemplateOrder counts the templates registered, under
	// resourcesMu: among templates matching a URI, the earlier registered
	// wins between equal literal prefixes.
	resourceTemplateOrder      uint64
	prompts                    map[string]mcp.Prompt
	promptHandlers             map[string]PromptHandlerFunc
	tools                      map[string]ServerTool
	taskTools                  map[string]ServerTaskTool
	toolHandlerMiddlewares     []ToolHandlerMiddleware
	taskToolHandlerMiddlewares []TaskToolHandlerMiddleware
	resourceHandlerMiddlewares []ResourceHandlerMiddleware
	promptHandlerMiddlewares   []PromptHandlerMiddleware
	toolFilters                []ToolFilterFunc
	promptFilters              []PromptFilterFunc
	notificationHandlers       map[string]NotificationHandlerFunc
	promptCompletionProvider   PromptCompletionProvider
	resourceCompletionProvider ResourceCompletionProvider
	capabilities               serverCapabilities
	cacheHints                 map[mcp.MCPMethod]cacheHints
	// toolHeaderBindings holds each registered tool's x-mcp-header bindings,
	// extracted once at registration so a tools/call never re-reads the input
	// schema. Keyed like tools and taskTools and kept in step with them under
	// toolsMu.
	toolHeaderBindings       map[string][]mcp.ParamHeaderBinding
	paginationLimit          *int
	toolResultSizeLimit      int
	sessions                 sync.Map
	listenSessions           sync.Map // see registerListenSession
	hooks                    *Hooks
	taskHooks                *TaskHooks
	tasks                    map[string]*taskEntry
	expiredTasks             map[string]string // Recently expired task IDs, each with its session
	maxConcurrentTasks       *int              // Optional limit on concurrent running tasks
	taskTTL                  time.Duration     // The longest a task is kept; see WithTaskTTL
	activeTasks              int               // Tasks whose handler may still be running
	inflightCancels          sync.Map          // inflightID -> *inflightRequest, the requests in flight
	inputValidator           *inputSchemaValidator
	outputValidator          *outputSchemaValidator
	strictInputSchemaDefault bool
	tracer                   tracing.Tracer
	propagator               tracing.Propagator
	metaPropagator           tracing.MetaPropagator
	requestLogger            *slog.Logger
}

// serverCapabilities defines the supported features of the MCP server
type serverCapabilities struct {
	tools        *toolCapabilities
	resources    *resourceCapabilities
	prompts      *promptCapabilities
	logging      *bool
	tasks        *taskCapabilities
	completions  *bool
	experimental map[string]any
	extensions   map[string]any
}

// resourceCapabilities defines the supported resource-related features
type resourceCapabilities struct {
	listChanged bool
}

// promptCapabilities defines the supported prompt-related features
type promptCapabilities struct {
	listChanged bool
}

// toolCapabilities defines the supported tool-related features
type toolCapabilities struct {
	listChanged bool
}

// taskCapabilities defines the supported task-related features
type taskCapabilities struct {
	list          bool
	cancel        bool
	toolCallTasks bool
}

// NewMCPServer creates a new MCP server instance with the given name, version and options
func NewMCPServer(
	name, version string,
	opts ...ServerOption,
) *MCPServer {
	s := &MCPServer{
		resources:                  make(map[string]resourceEntry),
		resourceTemplates:          make(map[string]resourceTemplateEntry),
		prompts:                    make(map[string]mcp.Prompt),
		promptHandlers:             make(map[string]PromptHandlerFunc),
		tools:                      make(map[string]ServerTool),
		taskTools:                  make(map[string]ServerTaskTool),
		toolHeaderBindings:         make(map[string][]mcp.ParamHeaderBinding),
		toolHandlerMiddlewares:     make([]ToolHandlerMiddleware, 0),
		resourceHandlerMiddlewares: make([]ResourceHandlerMiddleware, 0),
		promptHandlerMiddlewares:   make([]PromptHandlerMiddleware, 0),
		name:                       name,
		version:                    version,
		notificationHandlers:       make(map[string]NotificationHandlerFunc),
		tasks:                      make(map[string]*taskEntry),
		expiredTasks:               make(map[string]string),
		promptCompletionProvider:   &DefaultPromptCompletionProvider{},
		resourceCompletionProvider: &DefaultResourceCompletionProvider{},
		capabilities: serverCapabilities{
			tools:       nil,
			resources:   nil,
			prompts:     nil,
			logging:     nil,
			tasks:       nil,
			completions: nil,
		},
		tracer:     tracing.NoopTracer(),
		propagator: tracing.NoopPropagator(),
	}

	for _, opt := range opts {
		opt(s)
	}
	// The identity is final once the options ran. Boxed here, every modern
	// result carries it without boxing its own copy: a boxed struct is copied
	// to the heap.
	s.serverInfo = s.serverImplementation()

	return s
}

// GenerateInProcessSessionID generates a unique session ID for inprocess clients
func (s *MCPServer) GenerateInProcessSessionID() string {
	return GenerateInProcessSessionID()
}

// Register tool capabilities due to a tool being added.  Default to
// listChanged: true, but don't change the value if we've already explicitly
// registered tools.listChanged false.
func (s *MCPServer) implicitlyRegisterToolCapabilities() {
	s.implicitlyRegisterCapabilities(
		func() bool { return s.capabilities.tools != nil },
		func() { s.capabilities.tools = &toolCapabilities{listChanged: true} },
	)
}

func (s *MCPServer) implicitlyRegisterResourceCapabilities() {
	s.implicitlyRegisterCapabilities(
		func() bool { return s.capabilities.resources != nil },
		func() { s.capabilities.resources = &resourceCapabilities{} },
	)
}

func (s *MCPServer) implicitlyRegisterPromptCapabilities() {
	s.implicitlyRegisterCapabilities(
		func() bool { return s.capabilities.prompts != nil },
		func() { s.capabilities.prompts = &promptCapabilities{} },
	)
}

func (s *MCPServer) implicitlyRegisterCapabilities(check func() bool, register func()) {
	s.capabilitiesMu.RLock()
	if check() {
		s.capabilitiesMu.RUnlock()
		return
	}
	s.capabilitiesMu.RUnlock()

	s.capabilitiesMu.Lock()
	if !check() {
		register()
	}
	s.capabilitiesMu.Unlock()
}

// AddNotificationHandler registers a new handler for incoming notifications
func (s *MCPServer) AddNotificationHandler(
	method string,
	handler NotificationHandlerFunc,
) {
	s.notificationHandlersMu.Lock()
	defer s.notificationHandlersMu.Unlock()
	s.notificationHandlers[method] = handler
}

// serverCapabilities builds the capability set advertised by this server.
// It is shared by the legacy initialize handshake and the modern
// server/discover RPC.
func (s *MCPServer) serverCapabilitiesSnapshot() mcp.ServerCapabilities {
	// Capabilities are registered at runtime - AddTool implicitly enables the
	// tools capability, for example - so reads must be synchronized against
	// those writers.
	s.capabilitiesMu.RLock()
	defer s.capabilitiesMu.RUnlock()

	capabilities := mcp.ServerCapabilities{}

	// Only add resource capabilities if they're configured. subscribe is
	// never advertised: nothing emits notifications/resources/updated.
	if s.capabilities.resources != nil {
		capabilities.Resources = &struct {
			Subscribe   bool `json:"subscribe,omitempty"`
			ListChanged bool `json:"listChanged,omitempty"`
		}{
			ListChanged: s.capabilities.resources.listChanged,
		}
	}

	// Only add prompt capabilities if they're configured
	if s.capabilities.prompts != nil {
		capabilities.Prompts = &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{
			ListChanged: s.capabilities.prompts.listChanged,
		}
	}

	// Only add tool capabilities if they're configured
	if s.capabilities.tools != nil {
		capabilities.Tools = &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{
			ListChanged: s.capabilities.tools.listChanged,
		}
	}

	if s.capabilities.logging != nil && *s.capabilities.logging {
		capabilities.Logging = &struct{}{}
	}

	// Only add task capabilities if they're configured
	if s.capabilities.tasks != nil {
		tasksCapability := &mcp.TasksCapability{}

		if s.capabilities.tasks.list {
			tasksCapability.List = &struct{}{}
		}

		if s.capabilities.tasks.cancel {
			tasksCapability.Cancel = &struct{}{}
		}

		if s.capabilities.tasks.toolCallTasks {
			tasksCapability.Requests = &mcp.TaskRequestsCapability{
				Tools: &struct {
					Call *struct{} `json:"call,omitempty"`
				}{
					Call: &struct{}{},
				},
			}
		}

		capabilities.Tasks = tasksCapability
	}

	if s.capabilities.completions != nil && *s.capabilities.completions {
		capabilities.Completions = &struct{}{}
	}

	if s.capabilities.experimental != nil {
		capabilities.Experimental = s.capabilities.experimental
	}

	if s.capabilities.extensions != nil {
		capabilities.Extensions = s.capabilities.extensions
	}

	return capabilities
}

// serverImplementation returns the identity this server reports to clients.
func (s *MCPServer) serverImplementation() mcp.Implementation {
	return mcp.Implementation{
		Name:        s.name,
		Version:     s.version,
		Title:       s.implementation.Title,
		Description: s.implementation.Description,
		WebsiteURL:  s.implementation.WebsiteURL,
		Icons:       s.implementation.Icons,
	}
}

func (s *MCPServer) handleInitialize(
	ctx context.Context,
	_ any,
	request mcp.InitializeRequest,
) (*mcp.InitializeResult, *requestError) {
	capabilities := s.serverCapabilitiesSnapshot()
	// A session with no ID is a request's own: nothing the server sends later
	// reaches it, so it is promised no list_changed notification, and it
	// cannot be told from another, so it is not offered tasks/list (Tasks.md
	// row 35; see listTasks). A session with an ID, such as stdio, is
	// delivered to and keeps both.
	if session := ClientSessionFromContext(ctx); session != nil && session.SessionID() == "" {
		if capabilities.Tools != nil {
			capabilities.Tools.ListChanged = false
		}
		if capabilities.Prompts != nil {
			capabilities.Prompts.ListChanged = false
		}
		if capabilities.Resources != nil {
			capabilities.Resources.ListChanged = false
		}
		if capabilities.Tasks != nil {
			capabilities.Tasks.List = nil
		}
	}

	result := mcp.InitializeResult{
		ProtocolVersion: s.protocolVersion(request.Params.ProtocolVersion),
		ServerInfo:      s.serverImplementation(),
		Capabilities:    capabilities,
		Instructions:    s.instructions,
	}

	if session := ClientSessionFromContext(ctx); session != nil {
		session.Initialize()

		// Store client info if the session supports it
		if sessionWithClientInfo, ok := session.(SessionWithClientInfo); ok {
			sessionWithClientInfo.SetClientInfo(request.Params.ClientInfo)
			sessionWithClientInfo.SetClientCapabilities(request.Params.Capabilities)
		}
		if versioned, ok := session.(sessionProtocolVersionSetter); ok {
			versioned.SetProtocolVersion(result.ProtocolVersion)
		}
	}

	return &result, nil
}

// protocolVersion returns the protocol version to report in an
// InitializeResult, given the version the client requested.
//
// The initialize handshake was removed in protocol version 2026-07-28, so the
// negotiated version is always capped at [mcp.LATEST_LEGACY_PROTOCOL_VERSION].
// Clients reach the modern, stateless protocol via server/discover instead.
func (s *MCPServer) protocolVersion(clientVersion string) string {
	return mcp.NegotiateLegacyVersion(clientVersion)
}

func (s *MCPServer) handlePing(
	_ context.Context,
	_ any,
	_ mcp.PingRequest,
) (*mcp.EmptyResult, *requestError) {
	return &mcp.EmptyResult{}, nil
}

// handleSetLevel keeps a legacy client's logging/setLevel on its session,
// when the session keeps one (SessionWithLogging), so the level it asked for
// is the one it is served at; a session that keeps none stays at
// legacyLogLevel. The level is validated first.
func (s *MCPServer) handleSetLevel(
	ctx context.Context,
	id any,
	request mcp.SetLevelRequest,
) (*mcp.EmptyResult, *requestError) {
	switch request.Params.Level {
	case mcp.LoggingLevelDebug, mcp.LoggingLevelInfo, mcp.LoggingLevelNotice,
		mcp.LoggingLevelWarning, mcp.LoggingLevelError, mcp.LoggingLevelCritical,
		mcp.LoggingLevelAlert, mcp.LoggingLevelEmergency:
		if session, ok := ClientSessionFromContext(ctx).(SessionWithLogging); ok {
			session.SetLogLevel(request.Params.Level)
		}
		return &mcp.EmptyResult{}, nil
	}
	return nil, &requestError{
		id:   id,
		code: mcp.INVALID_PARAMS,
		err:  fmt.Errorf("invalid logging level '%s'", request.Params.Level),
	}
}

func (s *MCPServer) handleNotification(
	ctx context.Context,
	notification mcp.JSONRPCNotification,
	message json.RawMessage,
) mcp.JSONRPCMessage {
	// Handle cancellation notifications per MCP spec. The request is named by
	// its exact id, read from message as the request's own key was.
	if notification.Method == string(mcp.MethodNotificationCancelled) {
		var cancelled struct {
			Params struct {
				RequestID json.RawMessage `json:"requestId"`
			} `json:"params"`
		}
		if json.Unmarshal(message, &cancelled) == nil && len(cancelled.Params.RequestID) > 0 {
			id, valid := requestIDValue(cancelled.Params.RequestID)
			if key, ok := inflightKey(ctx, id); valid && ok {
				if request, loaded := s.inflightCancels.LoadAndDelete(key); loaded {
					request.(*inflightRequest).fire()
				}
			}
		}
		return nil
	}

	s.notificationHandlersMu.RLock()
	handler, ok := s.notificationHandlers[notification.Method]
	s.notificationHandlersMu.RUnlock()

	if ok {
		handler(ctx, notification)
	}
	return nil
}

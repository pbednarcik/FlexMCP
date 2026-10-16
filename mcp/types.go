// Package mcp defines the core types and interfaces for the Model Context Protocol (MCP).
// MCP is a protocol for communication between LLM-powered applications and their supporting services.
package mcp

import (
	"encoding/json"
	"net/http"

	"github.com/yosida95/uritemplate/v3"
)

type MCPMethod string

const (
	// MethodInitialize initiates connection and negotiates protocol capabilities.
	// https://modelcontextprotocol.io/specification/2024-11-05/basic/lifecycle/#initialization
	//
	// Removed in protocol version 2026-07-28 (SEP-2575), which carries the
	// protocol version, client identity, and client capabilities in each
	// request's _meta instead. Servers reject it on requests declaring that
	// version or later; use [MethodServerDiscover] there.
	MethodInitialize MCPMethod = "initialize"

	// MethodPing verifies connection liveness between client and server.
	// https://modelcontextprotocol.io/specification/2024-11-05/basic/utilities/ping/
	//
	// Removed in protocol version 2026-07-28 (SEP-2575), where liveness is a
	// transport concern. Servers reject it on requests declaring that version
	// or later.
	MethodPing MCPMethod = "ping"

	// MethodResourcesList lists all available server resources.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/resources/
	MethodResourcesList MCPMethod = "resources/list"

	// MethodResourcesTemplatesList provides URI templates for constructing resource URIs.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/resources/
	MethodResourcesTemplatesList MCPMethod = "resources/templates/list"

	// MethodResourcesRead retrieves content of a specific resource by URI.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/resources/
	MethodResourcesRead MCPMethod = "resources/read"

	// MethodResourcesSubscribe subscribes the client to updates for a resource.
	// https://modelcontextprotocol.io/specification/2025-11-25/server/resources
	//
	// Removed in protocol version 2026-07-28 (SEP-2575). Use the
	// ResourceSubscriptions field of a [MethodSubscriptionsListen] request
	// there.
	MethodResourcesSubscribe MCPMethod = "resources/subscribe"

	// MethodResourcesUnsubscribe cancels a previous resources/subscribe request.
	// https://modelcontextprotocol.io/specification/2025-11-25/server/resources
	//
	// Removed in protocol version 2026-07-28 (SEP-2575). Close the
	// [MethodSubscriptionsListen] stream instead.
	MethodResourcesUnsubscribe MCPMethod = "resources/unsubscribe"

	// MethodPromptsList lists all available prompt templates.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/prompts/
	MethodPromptsList MCPMethod = "prompts/list"

	// MethodPromptsGet retrieves a specific prompt template with filled parameters.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/prompts/
	MethodPromptsGet MCPMethod = "prompts/get"

	// MethodToolsList lists all available executable tools.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/tools/
	MethodToolsList MCPMethod = "tools/list"

	// MethodToolsCall invokes a specific tool with provided parameters.
	// https://modelcontextprotocol.io/specification/2024-11-05/server/tools/
	MethodToolsCall MCPMethod = "tools/call"

	// MethodSetLogLevel configures the minimum log level for client
	// https://modelcontextprotocol.io/specification/2025-03-26/server/utilities/logging
	//
	// Removed in protocol version 2026-07-28 (SEP-2575). Set [MetaKeyLogLevel]
	// in each request's _meta instead.
	MethodSetLogLevel MCPMethod = "logging/setLevel"

	// MethodElicitationCreate requests additional information from the user during interactions.
	// https://modelcontextprotocol.io/docs/concepts/elicitation
	MethodElicitationCreate MCPMethod = "elicitation/create"

	// MethodListRoots requests roots list from the client during interactions.
	// https://modelcontextprotocol.io/specification/2025-06-18/client/roots
	MethodListRoots MCPMethod = "roots/list"

	// MethodTasksGet retrieves the current status of a task.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks
	MethodTasksGet MCPMethod = "tasks/get"

	// MethodTasksList lists all tasks for the current session.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks
	MethodTasksList MCPMethod = "tasks/list"

	// MethodTasksResult retrieves the result of a completed task.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks
	MethodTasksResult MCPMethod = "tasks/result"

	// MethodTasksCancel cancels an in-progress task.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks
	MethodTasksCancel MCPMethod = "tasks/cancel"

	// MethodNotificationInitialized indicates that the client completed initialization.
	// https://modelcontextprotocol.io/specification/2024-11-05/basic/lifecycle/#initialization
	MethodNotificationInitialized MCPMethod = "notifications/initialized"

	// MethodNotificationCancelled cancels an in-flight request.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation
	MethodNotificationCancelled MCPMethod = "notifications/cancelled"

	// MethodNotificationProgress reports progress for a long-running request.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/progress
	MethodNotificationProgress MCPMethod = "notifications/progress"

	// MethodNotificationMessage is a server-pushed log message.
	// https://modelcontextprotocol.io/specification/2025-11-25/server/utilities/logging
	MethodNotificationMessage MCPMethod = "notifications/message"

	// MethodNotificationResourcesListChanged notifies when the list of available resources changes.
	// https://modelcontextprotocol.io/specification/2025-03-26/server/resources#list-changed-notification
	MethodNotificationResourcesListChanged = "notifications/resources/list_changed"

	// MethodNotificationResourceUpdated notifies a client that a resource it
	// listed in the resourceSubscriptions of subscriptions/listen has changed.
	MethodNotificationResourceUpdated = "notifications/resources/updated"

	// MethodNotificationPromptsListChanged notifies when the list of available prompt templates changes.
	// https://modelcontextprotocol.io/specification/2025-03-26/server/prompts#list-changed-notification
	MethodNotificationPromptsListChanged = "notifications/prompts/list_changed"

	// MethodNotificationToolsListChanged notifies when the list of available tools changes.
	// https://modelcontextprotocol.io/specification/2025-06-18/server/tools#list-changed-notification
	MethodNotificationToolsListChanged = "notifications/tools/list_changed"

	// MethodNotificationRootsListChanged notifies when the list of available roots changes.
	// https://modelcontextprotocol.io/specification/2025-06-18/client/roots#root-list-changes
	MethodNotificationRootsListChanged = "notifications/roots/list_changed"

	// MethodNotificationTasksStatus notifies when a task's status changes.
	// https://modelcontextprotocol.io/specification/2025-11-25/basic/utilities/tasks
	MethodNotificationTasksStatus = "notifications/tasks/status"

	// MethodCompletionComplete returns completion suggestions for a given argument
	// https://modelcontextprotocol.io/specification/2025-11-25/server/utilities/completion
	MethodCompletionComplete MCPMethod = "completion/complete"

	// MethodServerDiscover advertises the server's supported protocol versions,
	// capabilities, and identity. Servers implementing protocol version
	// 2026-07-28 or later MUST support it; it replaces the initialize handshake.
	// https://modelcontextprotocol.io/specification/2026-07-28/server/discover
	MethodServerDiscover MCPMethod = "server/discover"

	// MethodSubscriptionsListen opens a long-lived stream for server-to-client
	// notifications. It replaces the HTTP GET endpoint and the
	// resources/subscribe and resources/unsubscribe RPCs.
	// https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http
	MethodSubscriptionsListen MCPMethod = "subscriptions/listen"

	// MethodNotificationSubscriptionsAcknowledged reports which subscriptions a
	// server established in response to a subscriptions/listen request.
	MethodNotificationSubscriptionsAcknowledged = "notifications/subscriptions/acknowledged"
)

type URITemplate struct {
	*uritemplate.Template
}

func (t *URITemplate) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Raw())
}

func (t *URITemplate) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	template, err := uritemplate.New(raw)
	if err != nil {
		return err
	}
	t.Template = template
	return nil
}

/* JSON-RPC types */

// JSONRPCMessage represents either a JSONRPCRequest, JSONRPCNotification, JSONRPCResponse, or JSONRPCError
type JSONRPCMessage any

// Protocol version constants, negotiation helpers, and JSONRPC_VERSION live
// in version.go.

// ProgressToken is used to associate progress notifications with the original request.
type ProgressToken any

// Cursor is an opaque token used to represent a cursor for pagination.
type Cursor string

type Request struct {
	Method string        `json:"method"`
	Params RequestParams `json:"params,omitzero"`
}

type RequestParams struct {
	Meta *Meta `json:"_meta,omitempty"`
}

type Params map[string]any

type Notification struct {
	Method string             `json:"method"`
	Params NotificationParams `json:"params,omitzero"`
}

type NotificationParams struct {
	// This parameter name is reserved by MCP to allow clients and
	// servers to attach additional metadata to their notifications.
	Meta map[string]any `json:"_meta,omitempty"`

	// Additional fields can be added to this map
	AdditionalFields map[string]any `json:"-"`
}

// MarshalJSON implements custom JSON marshaling
func (p NotificationParams) MarshalJSON() ([]byte, error) {
	// Create a map to hold all fields
	m := make(map[string]any)

	// Add Meta if it exists
	if p.Meta != nil {
		m["_meta"] = p.Meta
	}

	// Add all additional fields
	for k, v := range p.AdditionalFields {
		// Ensure we don't override the _meta field
		if k != "_meta" {
			m[k] = v
		}
	}

	return json.Marshal(m)
}

// UnmarshalJSON implements custom JSON unmarshaling
func (p *NotificationParams) UnmarshalJSON(data []byte) error {
	// Create a map to hold all fields
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	// Initialize maps if they're nil
	if p.Meta == nil {
		p.Meta = make(map[string]any)
	}
	if p.AdditionalFields == nil {
		p.AdditionalFields = make(map[string]any)
	}

	// Process all fields
	for k, v := range m {
		if k == "_meta" {
			// Handle Meta field
			if meta, ok := v.(map[string]any); ok {
				p.Meta = meta
			}
		} else {
			// Handle additional fields
			p.AdditionalFields[k] = v
		}
	}

	return nil
}

type Result struct {
	// This result property is reserved by the protocol to allow clients and
	// servers to attach additional metadata to their responses.
	Meta *Meta `json:"_meta,omitempty"`

	// ResultType indicates how the client should interpret this result.
	//
	// Servers implementing protocol version 2026-07-28 or later MUST populate
	// it. It is omitted when responding to a request that used an earlier
	// protocol version; clients MUST treat an absent value as
	// [ResultTypeComplete].
	ResultType ResultType `json:"resultType,omitempty"`
}

/* Empty result */

// EmptyResult represents a response that indicates success but carries no data.
type EmptyResult Result

/* Cancellation */

// CancelledNotification can be sent by either side to indicate that it is
// cancelling a previously-issued request.
//
// The request SHOULD still be in-flight, but due to communication latency, it
// is always possible that this notification MAY arrive after the request has
// already finished.
//
// This notification indicates that the result will be unused, so any
// associated processing SHOULD cease.
//
// A client MUST NOT attempt to cancel its `initialize` request.
type CancelledNotification struct {
	Notification
	Params CancelledNotificationParams `json:"params"`
}

type CancelledNotificationParams struct {
	// The ID of the request to cancel.
	//
	// This MUST correspond to the ID of a request previously issued
	// in the same direction.
	RequestId RequestId `json:"requestId"`

	// An optional string describing the reason for the cancellation. This MAY
	// be logged or presented to the user.
	Reason string `json:"reason,omitempty"`
}

/* Ping */

// PingRequest represents a ping, issued by either the server or the client,
// to check that the other party is still alive. The receiver must promptly respond,
// or else may be disconnected.
type PingRequest struct {
	Request
	Header http.Header `json:"-"`
}

/* Progress notifications */

// ProgressNotification is an out-of-band notification used to inform the
// receiver of a progress update for a long-running request.
type ProgressNotification struct {
	Notification
	Params ProgressNotificationParams `json:"params"`
}

type ProgressNotificationParams struct {
	// The progress token which was given in the initial request, used to
	// associate this notification with the request that is proceeding.
	ProgressToken ProgressToken `json:"progressToken"`
	// The progress thus far. This should increase every time progress is made,
	// even if the total is unknown.
	Progress float64 `json:"progress"`
	// Total number of items to process (or total progress required), if known.
	Total float64 `json:"total,omitempty"`
	// Message related to progress. This should provide relevant human-readable
	// progress information.
	Message string `json:"message,omitempty"`
}

// UnmarshalJSON decodes the params with the progress token kept exact, as
// [Meta] decodes it from the request.
func (p *ProgressNotificationParams) UnmarshalJSON(data []byte) error {
	type params ProgressNotificationParams
	var decoded struct {
		params
		ProgressToken json.RawMessage `json:"progressToken"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = ProgressNotificationParams(decoded.params)
	if len(decoded.ProgressToken) == 0 {
		return nil
	}
	token, err := decodeProgressToken(decoded.ProgressToken)
	if err != nil {
		return err
	}
	p.ProgressToken = token
	return nil
}

/* Pagination */

type PaginatedRequest struct {
	Request
	Params PaginatedParams `json:"params,omitzero"`
}

type PaginatedParams struct {
	// An opaque token representing the current pagination position.
	// If provided, the server should return results starting after this cursor.
	// Absent and empty are two values: nil sends no cursor, an empty cursor
	// is sent as one.
	Cursor *Cursor `json:"cursor,omitempty"`
	// Meta carries protocol-level metadata. PaginatedRequest embeds Request
	// and shadows its Params with this type, so Meta must be declared here
	// to be marshaled on paginated requests (tools/list, resources/list,
	// resources/templates/list, prompts/list, tasks/list).
	Meta *Meta `json:"_meta,omitempty"`
}

type PaginatedResult struct {
	CacheableResult
	// An opaque token representing the pagination position after the last
	// returned result.
	// If present, there may be more results available: nil is the last page,
	// and an empty cursor is a cursor the next request carries.
	NextCursor *Cursor `json:"nextCursor,omitempty"`
}

// ClientRequest represents any request that can be sent from client to server.
type ClientRequest any

// ClientNotification represents any notification that can be sent from client to server.
type ClientNotification any

// ClientResult represents any result that can be sent from client to server.
type ClientResult any

// ServerRequest represents any request that can be sent from server to client.
type ServerRequest any

// ServerNotification represents any notification that can be sent from server to client.
type ServerNotification any

// ServerResult represents any result that can be sent from server to client.
type ServerResult any

type Named interface {
	GetName() string
}

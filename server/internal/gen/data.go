package main

type MCPRequestType struct {
	MethodName  string
	ParamType   string
	ResultType  string
	HookName    string
	Group       string
	GroupName   string
	HandlerFunc string
	ResultIsAny bool // If true, result type is 'any' instead of '*mcp.ResultType'
	// Decode, when set, is the server method that decodes the request in
	// place of json.Unmarshal(message, &request).
	Decode string
	// Settle, when set, is the server method run on the request once the
	// handler returns, before the hooks after the request see it.
	Settle string
	// RemovedInModern marks methods removed by protocol version 2026-07-28.
	// They are rejected with METHOD_NOT_FOUND on requests declaring that
	// version or later.
	RemovedInModern bool
	// RequiresModern marks methods introduced by protocol version 2026-07-28.
	// They are rejected with METHOD_NOT_FOUND on legacy requests.
	RequiresModern bool
}

var MCPRequestTypes = []MCPRequestType{
	{
		MethodName:      "MethodInitialize",
		ParamType:       "InitializeRequest",
		ResultType:      "InitializeResult",
		HookName:        "Initialize",
		HandlerFunc:     "handleInitialize",
		RemovedInModern: true,
	}, {
		MethodName:      "MethodPing",
		ParamType:       "PingRequest",
		ResultType:      "EmptyResult",
		HookName:        "Ping",
		HandlerFunc:     "handlePing",
		RemovedInModern: true,
	}, {
		MethodName:     "MethodServerDiscover",
		ParamType:      "DiscoverRequest",
		ResultType:     "DiscoverResult",
		HookName:       "Discover",
		HandlerFunc:    "handleDiscover",
		RequiresModern: true,
	}, {
		MethodName:     "MethodSubscriptionsListen",
		ParamType:      "SubscriptionsListenRequest",
		ResultType:     "SubscriptionsListenResult",
		HookName:       "SubscriptionsListen",
		HandlerFunc:    "handleSubscriptionsListen",
		RequiresModern: true,
	}, {
		MethodName:      "MethodSetLogLevel",
		ParamType:       "SetLevelRequest",
		ResultType:      "EmptyResult",
		Group:           "logging",
		GroupName:       "Logging",
		HookName:        "SetLevel",
		HandlerFunc:     "handleSetLevel",
		RemovedInModern: true,
	}, {
		MethodName:  "MethodResourcesList",
		ParamType:   "ListResourcesRequest",
		ResultType:  "ListResourcesResult",
		Group:       "resources",
		GroupName:   "Resources",
		HookName:    "ListResources",
		HandlerFunc: "handleListResources",
	}, {
		MethodName:  "MethodResourcesTemplatesList",
		ParamType:   "ListResourceTemplatesRequest",
		ResultType:  "ListResourceTemplatesResult",
		Group:       "resources",
		GroupName:   "Resources",
		HookName:    "ListResourceTemplates",
		HandlerFunc: "handleListResourceTemplates",
	}, {
		MethodName:  "MethodResourcesRead",
		ParamType:   "ReadResourceRequest",
		ResultType:  "ReadResourceResult",
		Group:       "resources",
		GroupName:   "Resources",
		HookName:    "ReadResource",
		HandlerFunc: "handleReadResource",
	}, {
		MethodName:  "MethodPromptsList",
		ParamType:   "ListPromptsRequest",
		ResultType:  "ListPromptsResult",
		Group:       "prompts",
		GroupName:   "Prompts",
		HookName:    "ListPrompts",
		HandlerFunc: "handleListPrompts",
	}, {
		MethodName:  "MethodPromptsGet",
		ParamType:   "GetPromptRequest",
		ResultType:  "GetPromptResult",
		Group:       "prompts",
		GroupName:   "Prompts",
		HookName:    "GetPrompt",
		HandlerFunc: "handleGetPrompt",
	}, {
		MethodName:  "MethodToolsList",
		ParamType:   "ListToolsRequest",
		ResultType:  "ListToolsResult",
		Group:       "tools",
		GroupName:   "Tools",
		HookName:    "ListTools",
		HandlerFunc: "handleListTools",
	}, {
		MethodName:  "MethodToolsCall",
		ParamType:   "CallToolRequest",
		ResultType:  "CallToolResult",
		Group:       "tools",
		GroupName:   "Tools",
		HookName:    "CallTool",
		HandlerFunc: "handleToolCall",
		ResultIsAny: true, // Returns 'any' to support both CallToolResult and CreateTaskResult
		Decode:      "decodeToolCall",
		Settle:      "settleToolCall",
	}, {
		MethodName:  "MethodTasksGet",
		ParamType:   "GetTaskRequest",
		ResultType:  "GetTaskResult",
		Group:       "tasks",
		GroupName:   "Tasks",
		HookName:    "GetTask",
		HandlerFunc: "handleGetTask",
	}, {
		MethodName:      "MethodTasksList",
		ParamType:       "ListTasksRequest",
		ResultType:      "ListTasksResult",
		Group:           "tasksList",
		GroupName:       "tasks.list",
		HookName:        "ListTasks",
		HandlerFunc:     "handleListTasks",
		RemovedInModern: true,
	}, {
		MethodName:      "MethodTasksResult",
		ParamType:       "TaskResultRequest",
		ResultType:      "TaskResultResult",
		Group:           "tasks",
		GroupName:       "Tasks",
		HookName:        "TaskResult",
		HandlerFunc:     "handleTaskResult",
		RemovedInModern: true,
	}, {
		MethodName:  "MethodTasksCancel",
		ParamType:   "CancelTaskRequest",
		ResultType:  "CancelTaskResult",
		Group:       "tasksCancel",
		GroupName:   "tasks.cancel",
		HookName:    "CancelTask",
		HandlerFunc: "handleCancelTask",
	}, {
		MethodName:  "MethodCompletionComplete",
		ParamType:   "CompleteRequest",
		ResultType:  "CompleteResult",
		Group:       "completions",
		GroupName:   "Completions",
		HookName:    "Complete",
		HandlerFunc: "handleComplete",
	},
}

<div align="center">

<img src=".github/assets/logo.svg" alt="FlexMCP" width="420">

**A high-performance Go implementation of the Model Context Protocol, tuned for busy servers that answer many complex tool calls.**

[![CI](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/pbednarcik/FlexMCP)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Linux](https://img.shields.io/badge/Linux-supported-2ea44f?logo=linux&logoColor=white)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![macOS](https://img.shields.io/badge/macOS-supported-2ea44f?logo=apple&logoColor=white)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![Windows](https://img.shields.io/badge/Windows-supported-2ea44f?logo=windows&logoColor=white)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)

</div>

## What FlexMCP is

FlexMCP began as a fork of [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go), the Go library for building MCP servers and clients, and has since been largely reworked: the server is stateless, the request path is rewritten for speed and allocations, and the code is checked against the MCP specification requirement by requirement. It is aimed at projects on the newer protocol revisions that want closer compliance with the MCP specification and fewer allocations per call: complex clients, and busy servers that answer many complex calls. It speaks the 2026-07-28 revision first and the earlier ones a client may still ask for, and where mcp-go was lenient it keeps the specification's MUSTs, from request ids to OAuth (HTTPS-only authorization servers, PKCE on by default, a token sent only to its own server). FlexMCP is optimized for speed and low allocations, so a busy server spends its time on its tools, not on the protocol. In our benchmarks a single in-process tool call is served up to 3 times faster than with mcp-go and up to 14 times faster than with the official Go SDK, with less than half of mcp-go's allocations ([benchmarks on two machines](www/docs/pages/performance.mdx)).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/assets/benchmark-mac-dark.svg">
  <img alt="Time per tools/call on an Apple M5 Max, lower is better. Small call, modern: FlexMCP 6.6 µs, mcp-go 21 µs, go-sdk 81 µs. Small, legacy: 4.0, 9.4, 56 µs. Complex call, modern: 67, 165, 302 µs. Complex, legacy: 63, 144, 200 µs." src=".github/assets/benchmark-mac-light.svg">
</picture>

## Long-running tool calls

FlexMCP serves the Tasks extension of the 2026-07-28 revision (`io.modelcontextprotocol/tasks`), from v0.3.0. A tool that runs long answers its call with a task, and the client polls it until the result is ready, with no connection held open and no session state. Task ids are sealed with an HMAC and can be bound to the caller, so a server keeps no table of ids and one caller cannot read another's tasks. Your own handler owns the work, or `MemoryTasks` runs it in memory. The client side polls, cancels and answers input. See [Tasks](www/docs/pages/servers/tasks.mdx) and `examples/tasks_extension`.

```go
tasks := server.NewMemoryTasks()
defer tasks.Close()

srv := server.NewMCPServer("report-server", "1.0.0",
	server.WithToolCapabilities(true),
	server.WithTaskHandler(tasks),
)
srv.AddTool(mcp.NewTool("report"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Run answers with a task when the client takes one, and runs the work
	// right here when it cannot.
	return tasks.Run(ctx, func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
		status("building the report")
		// ... the long work, returning when ctx is done ...
		return mcp.NewToolResultText("the report is ready"), nil
	})
})
```

A client declares the extension, calls the tool, and waits for the task:

```go
c := client.NewClient(transport.NewInProcessTransport(srv),
	client.WithClientCapabilities(mcp.ClientCapabilities{
		Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}},
	}),
)
// ... Start and Initialize ...

var call mcp.CallToolRequest
call.Params.Name = "report"
result, err := c.CallTool(ctx, call)
if err == nil && result.Task != nil {
	// Polls tasks/get at the interval the server suggests, until the task ends.
	state, err := c.WaitForExtensionTask(ctx, result.Task.TaskId, time.Second)
	// ... state.TaskResult holds the result of a completed task ...
}
```

## Coming from mcp-go

Most code carries over unchanged, but some client APIs and behaviours changed. Before upgrading, read [Client changes](www/docs/pages/protocol-2026-07-28.mdx#client-changes) and [Request ids and progress tokens](www/docs/pages/protocol-2026-07-28.mdx#request-ids-and-progress-tokens).

## Install

```bash
go get github.com/pbednarcik/flexmcp
```

The OpenTelemetry adapter is a module of its own:

```bash
go get github.com/pbednarcik/flexmcp/otel
```

Releases and their notes are on the [releases page](https://github.com/pbednarcik/FlexMCP/releases).

## Contributing

Issues and pull requests are welcome; FlexMCP is maintained in spare time, so replies can take a while. See [CONTRIBUTING.md](CONTRIBUTING.md). If a bug also happens in mcp-go, it usually belongs [upstream](https://github.com/mark3labs/mcp-go/issues) first.

## License and thanks

MIT, see [LICENSE](LICENSE). FlexMCP started from [mcp-go](https://github.com/mark3labs/mcp-go) by mark3labs and its contributors, and much of it is still their work. Thank you.

<div align="center">

<img src=".github/assets/logo.svg" alt="FlexMCP" width="420">

**A Go implementation of the Model Context Protocol, tuned for busy servers that answer many complex tool calls.**

[![CI](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

</div>

## What FlexMCP is

FlexMCP is a fork of [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go), the Go library for building MCP servers and clients. It keeps mcp-go's API and adds the changes a very busy MCP server needs, one that answers many complex calls, starting with fewer allocations on the hot request path. Next comes functionality some tools need that mcp-go does not offer yet, such as server notifications (list changes, progress) on the 2026-07-28 protocol and, later, long-running tool calls through the Tasks extension.

## Install

FlexMCP has no release yet. The first one, `v0.1.0`, will be installed with:

```bash
go get github.com/pbednarcik/flexmcp
```

## Quick start

A server with one tool, served over stdio:

```go
package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("Demo", "1.0.0", server.WithToolCapabilities(false))

	tool := mcp.NewTool("hello_world",
		mcp.WithDescription("Say hello to someone"),
		mcp.WithString("name", mcp.Required(), mcp.Description("Name of the person to greet")),
	)
	s.AddTool(tool, helloHandler)

	if err := server.ServeStdio(s); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}

func helloHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := request.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Hello, %s!", name)), nil
}
```

FlexMCP covers everything mcp-go does: tools, resources, prompts, sampling, elicitation, completions, tasks, and the stdio, SSE, streamable HTTP and in-process transports. mcp-go's guide at [mcp-go.dev](https://mcp-go.dev) applies as written, and the [`examples/`](examples) folder has runnable programs.

## Contributing

Issues and pull requests are welcome; FlexMCP is maintained in spare time, so replies can take a while. See [CONTRIBUTING.md](CONTRIBUTING.md). If a bug also happens in mcp-go, it usually belongs [upstream](https://github.com/mark3labs/mcp-go/issues) first.

## License and thanks

MIT, see [LICENSE](LICENSE). FlexMCP is built on [mcp-go](https://github.com/mark3labs/mcp-go) by mark3labs and its contributors; nearly all of the code here is theirs. Thank you.

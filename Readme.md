<div align="center">

<img src=".github/assets/logo.svg" alt="FlexMCP" width="420">

**A Go implementation of the Model Context Protocol, tuned for busy servers that answer many complex tool calls.**

[![CI](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/pbednarcik/FlexMCP)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Linux](https://img.shields.io/badge/Linux-supported-2ea44f?logo=linux&logoColor=white)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![macOS](https://img.shields.io/badge/macOS-supported-2ea44f?logo=apple&logoColor=white)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)
[![Windows](https://img.shields.io/badge/Windows-supported-2ea44f?logo=windows&logoColor=white)](https://github.com/pbednarcik/FlexMCP/actions/workflows/ci.yml)

</div>

## What FlexMCP is

FlexMCP is a fork of [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go), the Go library for building MCP servers and clients. It keeps mcp-go's API and adds the changes a very busy MCP server needs, one that answers many complex calls, starting with fewer allocations on the hot request path. Next comes functionality some tools need that mcp-go does not offer yet, such as server notifications (list changes, progress) on the 2026-07-28 protocol and, later, long-running tool calls through the Tasks extension.

## Install

FlexMCP has no release yet. The first one, `v0.1.0`, will be installed with:

```bash
go get github.com/pbednarcik/flexmcp
```

## Contributing

Issues and pull requests are welcome; FlexMCP is maintained in spare time, so replies can take a while. See [CONTRIBUTING.md](CONTRIBUTING.md). If a bug also happens in mcp-go, it usually belongs [upstream](https://github.com/mark3labs/mcp-go/issues) first.

## License and thanks

MIT, see [LICENSE](LICENSE). FlexMCP is built on [mcp-go](https://github.com/mark3labs/mcp-go) by mark3labs and its contributors; nearly all of the code here is theirs. Thank you.

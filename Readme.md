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

FlexMCP is a fork of [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go), the Go library for building MCP servers and clients. It is aimed at projects on the newer protocol revisions that want closer compliance with the MCP specification and fewer allocations per call: complex clients, and busy servers that answer many complex calls. It speaks the 2026-07-28 revision first and the earlier ones a client may still ask for, and where mcp-go was lenient it keeps the specification's MUSTs, from request ids to OAuth (HTTPS-only authorization servers, PKCE always on, a token sent only to its own server). Cutting allocations on the hot request path is the work in progress, and long-running tool calls through the Tasks extension come after.

## Coming from mcp-go

Most code carries over unchanged, but some client APIs and behaviours changed. Before upgrading, read [Client changes](www/docs/pages/protocol-2026-07-28.mdx#client-changes) and [Request ids and progress tokens](www/docs/pages/protocol-2026-07-28.mdx#request-ids-and-progress-tokens).

## Install

FlexMCP has no release yet. The first one, `v0.1.0`, will be installed with:

```bash
go get github.com/pbednarcik/flexmcp
```

## Contributing

Issues and pull requests are welcome; FlexMCP is maintained in spare time, so replies can take a while. See [CONTRIBUTING.md](CONTRIBUTING.md). If a bug also happens in mcp-go, it usually belongs [upstream](https://github.com/mark3labs/mcp-go/issues) first.

## License and thanks

MIT, see [LICENSE](LICENSE). FlexMCP is built on [mcp-go](https://github.com/mark3labs/mcp-go) by mark3labs and its contributors; nearly all of the code here is theirs. Thank you.

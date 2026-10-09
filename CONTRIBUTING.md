# Contributing

Thanks for your interest in FlexMCP.

**A note on response times:** FlexMCP is maintained by one person in spare time. Issues and pull requests are welcome, but replies and reviews can take a while, sometimes weeks. If something is urgent for you, a fork is the fastest path.

## Before you start

FlexMCP is a fork of [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go). If a bug or feature applies to mcp-go as well, it usually belongs upstream first; FlexMCP takes upstream changes in from time to time.

For anything larger than a small fix, please open an issue first so we can agree on the approach before you spend time on it.

## Development

Go 1.25 or later. The repository holds two modules: the core at the root, and the OpenTelemetry adapter in `otel/` with its own `go.mod`.

```bash
go test ./... -race
cd otel && go test ./... -race
golangci-lint run
```

`go generate ./...` regenerates the request handler and hooks in `server/` from the templates in `server/internal/gen/`; run it after changing a template and commit the result.

## Pull requests

- One change per pull request, with a test that fails without it.
- A change that claims a performance effect comes with a benchmark and its before and after numbers (benchstat).
- Commit messages follow `type: summary` (`fix:`, `feat:`, `perf:`, `docs:`, `test:`, `chore:`).
- Pull requests target `master`.

## Releasing

Tag the core module first, then point `otel/go.mod` at the new core tag (dropping the development `replace`), then tag the submodule:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z

cd otel
go mod edit -require=<core module path>@vX.Y.Z -dropreplace=<core module path>
go mod tidy
git commit -m "otel: pin core to vX.Y.Z" -- go.mod go.sum
git tag otel/vX.Y.Z
git push origin otel/vX.Y.Z
```

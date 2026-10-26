# The scoreboard: FlexMCP, go-sdk and mcp-go

The same tools/call, served by each library's Streamable HTTP handler, timed by one harness.

| Module | What |
|---|---|
| `harness/` | The shared requests (`testdata/`, written by `go run ./gen`), the checks and the timing legs |
| `flexmcp/` | FlexMCP, the working tree of this repository |
| `mcpgo/` | `github.com/mark3labs/mcp-go` v1.3.0, upstream |
| `gosdk/` | `github.com/modelcontextprotocol/go-sdk` v1.8.0, the official SDK |
| `run/` | Runs the three and compares them with benchstat |

## Legs

`BenchmarkCompare/<era>/<payload>/<mode>`:

- **era**: `modern` is a sessionless 2026-07-28 call with `_meta` and the standard headers; `legacy` is a 2025-11-25 call without a session.
- **payload**: `small` is a one-field schema and a short text result; `complex` is an 8 KB schema with input validation on, 20 nested items and 40 more arguments, and a ~16 KB text result.
- **mode**: `serial` calls the handler's `ServeHTTP` one call at a time; `parallel` from every core at once; `loopback` (small only) through a real `net/http` server on 127.0.0.1 with a plain `http.Client`.

## Fairness rules

1. Every library gets the same request bytes and headers (`harness/testdata`).
2. Each library is set up the way its own docs set up a production server, sessionless with JSON answers and logging off: FlexMCP as it ships (it has no legacy sessions), mcp-go with `WithStateLess(true)`, go-sdk with `Stateless: true, JSONResponse: true` (the only go-sdk mode that serves 2026-07-28).
3. Input validation: on for `complex` in all three. On `small`, each library's default: go-sdk validates, FlexMCP and mcp-go do not.
4. No leg is timed until it has answered the tool's result once; a `complex` leg must also refuse an invalid argument (`go test ./...` in each library's module).
5. Rival versions are pinned and bumped by hand, with a re-run.

## Running it

From the repository root:

    go run -C bench/compare/run .

It builds each library's benchmarks once, then runs ten rounds, each running every library once in an order that rotates per round, so a machine that drifts slows every library alike (about 25 minutes). It writes the raw results and the benchstat comparison under `results/<date>/`. Timings need a quiet machine: close other work first.

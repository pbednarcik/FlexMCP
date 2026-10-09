## What and why

<!-- What this changes and the problem it solves. Link the issue: Fixes #123 -->

## Measurement

<!-- For a change that claims a performance effect: the benchmark, before and after (benchstat output).
     Delete this section if the change makes no performance claim. -->

## Upstream

<!-- Could mark3labs/mcp-go take this too? Link the upstream PR or issue, or say why it is FlexMCP-only. -->

## Checklist

- [ ] Tests cover the change (a failing test first for a bug fix)
- [ ] `go test ./... -race` passes (and in `otel/` if it is touched)
- [ ] `golangci-lint run` is clean
- [ ] `go generate ./...` leaves no diff, if a generator template changed

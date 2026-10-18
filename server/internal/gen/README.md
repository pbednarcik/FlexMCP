# Readme for Codegen

This internal module contains code generation for producing a few repetitive
constructs, namely:

- The switch statement that handles the request dispatch
- The hook function types and the methods on the Hook struct

To invoke the code generation:

```
go generate ./...
```

## Development

- `request_handler.go.tmpl` generates `server/request_handler.go`,
- `hooks.go.tmpl` generates `server/hooks.go`, and
- `dispatch_gates_test.go.tmpl` generates `server/dispatch_gates_gen_test.go`

Inside of `data.go` there is a struct with the inputs to the templates.

The driver in `main.go` formats what a template renders with `go/format`
and nothing else: each template lists exactly the imports its output uses,
so a template that adds or drops one must change its import list too, or the
generated file does not build.


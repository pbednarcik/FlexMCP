module github.com/pbednarcik/flexmcp/otel

go 1.27.2

toolchain go1.27.4

require (
	github.com/pbednarcik/flexmcp v0.2.0
	github.com/stretchr/testify v1.12.1
	go.opentelemetry.io/contrib/bridges/otelslog v0.21.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/log v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/log v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/pbednarcik/flexmcp => ../

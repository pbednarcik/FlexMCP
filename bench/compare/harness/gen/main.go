// Command gen writes the scoreboard's shared schemas and requests into
// testdata/, so every library is measured with the same bytes. Run it from
// bench/compare/harness: go run ./gen
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

const region = "us-east-1"

func main() {
	write("schema-small.json", []byte(`{"type":"object","properties":{"region":{"type":"string"}},"required":["region"]}`))
	write("schema-complex.json", complexSchema(8192))
	small := map[string]any{"region": region}
	complexArgs := complexArguments()
	for _, era := range []string{"modern", "legacy"} {
		write(era+"-small.json", request(era, small))
		write(era+"-complex.json", request(era, complexArgs))
	}
}

// complexSchema is an object schema of at least size bytes: region, an array
// of nested items, and string properties p000... as filler.
func complexSchema(size int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"object","required":["region","items"],"properties":{`)
	b.WriteString(`"region":{"type":"string"},`)
	b.WriteString(`"items":{"type":"array","items":{"type":"object","required":["id","name"],"properties":{`)
	b.WriteString(`"id":{"type":"integer"},"name":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}}}}`)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, `,"p%03d":{"type":"string","description":"property number %03d"}`, i, i)
	}
	b.WriteString(`}}`)
	if !json.Valid([]byte(b.String())) {
		log.Fatal("complex schema is not valid JSON")
	}
	return []byte(b.String())
}

// complexArguments satisfies complexSchema: 20 nested items and 40 filler
// strings beside the region.
func complexArguments() map[string]any {
	args := map[string]any{"region": region}
	items := make([]any, 20)
	for i := range items {
		items[i] = map[string]any{"id": i, "name": fmt.Sprintf("item-%02d", i), "tags": []string{"alpha", "beta", "gamma"}}
	}
	args["items"] = items
	for i := range 40 {
		args[fmt.Sprintf("p%03d", i)] = fmt.Sprintf("value %03d", i)
	}
	return args
}

// request is one tools/call. A modern request carries the 2026-07-28 _meta
// keys; a legacy one carries none.
func request(era string, args map[string]any) []byte {
	params := map[string]any{"name": "echo", "arguments": args}
	if era == "modern" {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "bench-client", "version": "1.0.0"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if err != nil {
		log.Fatal(err)
	}
	return body
}

func write(name string, data []byte) {
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", name), data, 0o644); err != nil {
		log.Fatal(err)
	}
}

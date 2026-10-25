package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The payloads below are bench/compare's small and complex tools/call, shared
// by the tests and benchmarks that measure them.

// smallSchema and smallArguments are bench/compare's small input schema and
// the arguments of its small call.
const (
	smallSchema    = `{"type":"object","properties":{"region":{"type":"string"}},"required":["region"]}`
	smallArguments = `{"region":"us-east-1"}`
)

// complexRawSchema is bench/compare's complex input schema, which
// complexArguments satisfy: the region, an array of nested items, and string
// properties p000... as filler, to about 8 KB.
func complexRawSchema() json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"type":"object","required":["region","items"],"properties":{"region":{"type":"string"},`)
	b.WriteString(`"items":{"type":"array","items":{"type":"object","required":["id","name"],"properties":{`)
	b.WriteString(`"id":{"type":"integer"},"name":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}}}}`)
	for i := 0; b.Len() < 8192; i++ {
		fmt.Fprintf(&b, `,"p%03d":{"type":"string","description":"property number %03d"}`, i, i)
	}
	b.WriteString(`}}`)
	return json.RawMessage(b.String())
}

// complexResultFiller makes a complex call's result about 16 KB, as
// bench/compare's is.
var complexResultFiller = ":" + strings.Repeat("0123456789abcdef", 1024)

// complexItem and complexTypedArgs are what a typed handler binds of
// bench/compare's complex call.
type complexItem struct {
	ID   int      `json:"id"`
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type complexTypedArgs struct {
	Region string        `json:"region"`
	Items  []complexItem `json:"items"`
}

// complexArguments are bench/compare's complex call's arguments: 20 nested
// items and 40 filler strings beside the region.
func complexArguments() json.RawMessage {
	var args strings.Builder
	args.WriteString(`{"items":[`)
	for i := range 20 {
		if i > 0 {
			args.WriteByte(',')
		}
		fmt.Fprintf(&args, `{"id":%d,"name":"item-%02d","tags":["alpha","beta","gamma"]}`, i, i)
	}
	args.WriteByte(']')
	for i := range 40 {
		fmt.Fprintf(&args, `,"p%03d":"value %03d"`, i, i)
	}
	args.WriteString(`,"region":"us-east-1"}`)
	return json.RawMessage(args.String())
}

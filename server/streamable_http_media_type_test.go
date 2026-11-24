package server

import (
	"mime"
	"testing"

	"github.com/stretchr/testify/assert"
)

// mediaTypeSpellings are Content-Type values a peer may send: the exact
// types, the same in other cases, with parameters good and bad, with
// whitespace, and near misses.
var mediaTypeSpellings = []string{
	"application/json", "Application/JSON", "APPLICATION/JSON",
	"application/json; charset=utf-8", "application/json;charset=UTF-8", "application/json ; charset=utf-8",
	" application/json", "application/json ", "\tapplication/json", "application/json\t",
	"application/json;", "application/json; charset", "application/json; charset=", "application/json; =x",
	"application/json; charset=utf-8; charset=utf-8", `application/json; charset="utf-8"`,
	"application/jsonx", "application/json2", "application / json", "application/json, text/plain",
	"application/", "/json", "application", "", " ", "*/*", "application/*",
	"text/event-stream", "Text/Event-Stream", "text/event-stream; charset=utf-8", "text/event-stream;",
	" text/event-stream", "text/event-stream; a", "text/event-streams", "text/plain",
	"application/json\x00", "application/jsön",
}

// A POST's Content-Type is taken or refused exactly as mime.ParseMediaType
// reads it, whatever its case, parameters and whitespace.
func TestJSONMediaTypeReadsAsParseMediaType(t *testing.T) {
	for _, contentType := range mediaTypeSpellings {
		mediaType, _, err := mime.ParseMediaType(contentType)
		want := err == nil && mediaType == "application/json"
		assert.Equal(t, want, isJSONMediaType(contentType), "Content-Type %q", contentType)
	}
}

// The type every client sends is taken without parsing, which allocated.
func TestJSONMediaTypeAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = isJSONMediaType("application/json") }))
}

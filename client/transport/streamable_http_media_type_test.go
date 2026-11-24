package transport

import (
	"mime"
	"testing"

	"github.com/stretchr/testify/assert"
)

// mediaTypeSpellings are Content-Type values a server may answer with: the
// exact types, the same in other cases, with parameters good and bad, with
// whitespace, and near misses.
var mediaTypeSpellings = []string{
	"text/event-stream", "Text/Event-Stream", "TEXT/EVENT-STREAM",
	"text/event-stream; charset=utf-8", "text/event-stream;charset=UTF-8", "text/event-stream ; charset=utf-8",
	" text/event-stream", "text/event-stream ", "\ttext/event-stream", "text/event-stream\t",
	"text/event-stream;", "text/event-stream; charset", "text/event-stream; charset=", "text/event-stream; =x",
	"text/event-stream; a=1; a=1", `text/event-stream; charset="utf-8"`,
	"text/event-streams", "text/event", "text / event-stream", "text/event-stream, application/json",
	"application/json", "Application/JSON", "application/json; charset=utf-8", "application/json;",
	" application/json", "application/json; charset", "application/jsonx", "application/json, text/plain",
	"application/", "/json", "application", "", " ", "*/*", "text/plain", "text/event-stream\x00",
}

// An answer's Content-Type reads as mime.ParseMediaType reads it, a
// parameter it cannot read ignored, whatever its case, parameters and
// whitespace.
func TestMediaTypeOfReadsAsParseMediaType(t *testing.T) {
	for _, contentType := range mediaTypeSpellings {
		want, _, _ := mime.ParseMediaType(contentType)
		assert.Equal(t, want, mediaTypeOf(contentType), "Content-Type %q", contentType)
	}
}

// The two types a server answers with are read without parsing, which
// allocated.
func TestMediaTypeOfAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		assert.Zero(t, testing.AllocsPerRun(100, func() { _ = mediaTypeOf(contentType) }), contentType)
	}
}

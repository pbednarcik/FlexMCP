package transport

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Watching for Close costs a request no goroutine of its own.
func TestContextAwareOfClientCloseStartsNoGoroutine(t *testing.T) {
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp")
	require.NoError(t, err)

	const requests = 100
	before := runtime.NumGoroutine()
	cancels := make([]context.CancelFunc, 0, requests)
	for range requests {
		_, cancel := c.contextAwareOfClientClose(t.Context())
		cancels = append(cancels, cancel)
	}
	assert.Less(t, runtime.NumGoroutine()-before, requests/10, "goroutines for %d requests in flight", requests)
	for _, cancel := range cancels {
		cancel()
	}
}

// A request that ends lets go of its watch on Close: a transport that serves
// many requests does not keep one for each.
func TestContextAwareOfClientCloseLetsGoWhenTheRequestEnds(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp")
	require.NoError(t, err)

	cycle := func(n int) {
		for range n {
			_, cancel := c.contextAwareOfClientClose(t.Context())
			cancel()
		}
	}
	cycle(1000)
	before := heapInUse()
	cycle(50000)
	assert.Less(t, heapInUse()-before, int64(1<<20), "heap kept by 50000 ended requests")
	// The transport is live until here: a collected one would take what it
	// kept with it.
	runtime.KeepAlive(c)
}

// heapInUse is the live heap after a collection.
func heapInUse() int64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapInuse)
}

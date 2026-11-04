package transport

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The listener retries at the interval the transport was created with: it
// never reads the package variable from its own goroutine, which outlives
// Close and would race a later test setting it.
func TestContinuousListeningKeepsTheRetryIntervalFromConstruction(t *testing.T) {
	orig := retryInterval
	t.Cleanup(func() { retryInterval = orig })

	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	retryInterval = 5 * time.Millisecond
	trans, err := NewStreamableHTTP(srv.URL, WithContinuousListening())
	require.NoError(t, err)
	t.Cleanup(func() { _ = trans.Close() })

	retryInterval = time.Hour
	require.NoError(t, trans.Start(t.Context()))
	trans.initializedOnce.Do(func() { close(trans.initialized) })

	assert.Eventually(t, func() bool { return gets.Load() >= 3 }, 2*time.Second, 5*time.Millisecond,
		"the listener retried at the package variable's interval, not the transport's")
}

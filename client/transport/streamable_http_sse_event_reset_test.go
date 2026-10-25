package transport

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStreamableHTTP_ReadSSEEventTypeResetsAfterDatalessEvent pins that an
// event's type does not leak into the next one when the event carried no
// data: SSE resets the type on every dispatch, with data or without.
func TestStreamableHTTP_ReadSSEEventTypeResetsAfterDatalessEvent(t *testing.T) {
	c := &StreamableHTTP{}
	var events []string
	body := "event: ping\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"
	c.readSSE(t.Context(), io.NopCloser(strings.NewReader(body)), func(event, _ string) {
		events = append(events, event)
	})
	require.Equal(t, []string{"message"}, events)
}

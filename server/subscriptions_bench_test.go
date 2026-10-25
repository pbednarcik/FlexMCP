package server

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mark3labs/mcp-go/mcp"
)

// broadcastSession is a registered client session that records nothing.
type broadcastSession struct {
	id string
	ch chan mcp.JSONRPCNotification
}

func (s *broadcastSession) SessionID() string                                   { return s.id }
func (s *broadcastSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.ch }
func (s *broadcastSession) Initialize()                                         {}
func (s *broadcastSession) Initialized() bool                                   { return true }

// broadcaster returns a server with sessions registered, every tenth a
// modern one with a subscriptions/listen stream for tool list changes: a
// stdio session or, with inProcess, an in-process one. Stdio sessions all
// share one id, so they are reached as listen sessions are rather than
// registered. It returns too the channels the sessions are sent on, buffered
// size deep, or a hundred for a stdio or in-process session's.
func broadcaster(tb testing.TB, sessions, size int, inProcess bool) (*MCPServer, []<-chan mcp.JSONRPCNotification) {
	tb.Helper()
	srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(true))
	channels := make([]<-chan mcp.JSONRPCNotification, 0, sessions)
	for i := range sessions {
		if i%10 != 0 {
			base := &broadcastSession{id: "s" + strconv.Itoa(i), ch: make(chan mcp.JSONRPCNotification, size)}
			if err := srv.RegisterSession(tb.Context(), base); err != nil {
				tb.Fatal(err)
			}
			channels = append(channels, base.ch)
			continue
		}
		var streams SessionWithSubscriptionStreams
		var ch <-chan mcp.JSONRPCNotification
		if inProcess {
			own := NewInProcessSession("s" + strconv.Itoa(i))
			own.Initialize()
			streams, ch = own, own.ClientNotifications()
		} else {
			stdio := NewStdioServer(srv).session
			stdio.Initialize()
			streams, ch = stdio, stdio.notifications
		}
		streams.MarkModern()
		streams.OpenSubscriptionStream(int64(i), mcp.SubscriptionFilter{ToolsListChanged: true})
		if inProcess {
			if err := srv.RegisterSession(tb.Context(), streams); err != nil {
				tb.Fatal(err)
			}
		} else {
			srv.registerListenSession(streams)
		}
		channels = append(channels, ch)
	}
	return srv, channels
}

// BenchmarkBroadcastListChanged measures a tool list change broadcast to 0, 10
// and 1000 sessions, a tenth of them listening, of the stdio kind or in
// process, each drained as a transport's writer would. Each drainer stops on
// a channel of its own, so the drainers do not contend on one.
func BenchmarkBroadcastListChanged(b *testing.B) {
	for _, listening := range []string{"stdio", "inprocess"} {
		for _, sessions := range []int{0, 10, 1000} {
			b.Run(fmt.Sprintf("%s/sessions=%d", listening, sessions), func(b *testing.B) {
				srv, channels := broadcaster(b, sessions, 64, listening == "inprocess")
				for _, ch := range channels {
					done := make(chan struct{})
					b.Cleanup(func() { close(done) })
					go func() {
						for {
							select {
							case <-ch:
							case <-done:
								return
							}
						}
					}()
				}
				b.ReportAllocs()
				for b.Loop() {
					srv.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
				}
			})
		}
	}
}

// A list change broadcast to 1000 sessions, 100 of them listening, delivers
// to every one, tagged, allocating only for the in-process sessions, whose
// notifications reach user code and so get a _meta each: 300 allocations
// when every listening session's streams were copied and each
// notification's tag was built, none on stdio sessions with the streams read
// in place and a tag kept with each, 200 in process.
func TestBroadcastListChangedAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	const runs = 50
	for _, tc := range []struct {
		inProcess bool
		ceiling   float64
	}{{false, 0}, {true, 200}} {
		srv, channels := broadcaster(t, 1000, runs+1, tc.inProcess)
		allocs := testing.AllocsPerRun(runs, func() {
			srv.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
		})
		for i, ch := range channels {
			if !assert.Len(t, ch, runs+1, "session %d", i) {
				break
			}
		}
		got := <-channels[0]
		assert.Equal(t, int64(0), got.Params.Meta[mcp.MetaKeySubscriptionID], "a listening session's change is tagged")
		plain := <-channels[1]
		assert.Nil(t, plain.Params.Meta, "a handshake-era session's change is not")
		assert.LessOrEqual(t, allocs, tc.ceiling, "allocations per list change broadcast to 1000 sessions, in process %v", tc.inProcess)
	}
}

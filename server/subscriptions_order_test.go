package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// streamSession is a long-lived session, as a stdio or in-process one, that
// holds subscriptions/listen streams and records what it is sent.
type streamSession struct {
	subscriptionStreams
	notify chan mcp.JSONRPCNotification
}

func newStreamSession() *streamSession {
	s := &streamSession{notify: make(chan mcp.JSONRPCNotification, 16)}
	s.MarkModern()
	return s
}

func (s *streamSession) SessionID() string { return "stream" }
func (s *streamSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notify
}
func (s *streamSession) Initialize()       {}
func (s *streamSession) Initialized() bool { return true }

var _ SessionWithSubscriptionStreams = (*streamSession)(nil)

// openStream serves subscriptions/listen with id 7 for filter on session in
// the background, until the test ends.
func openStream(t *testing.T, srv *MCPServer, session ClientSession, filter mcp.SubscriptionFilter) {
	t.Helper()
	require.NoError(t, srv.RegisterSession(t.Context(), session))
	ctx, cancel := context.WithCancel(srv.WithContext(t.Context(), session))
	ctx = WithRequestProtocolInfo(ctx, &RequestProtocolInfo{Modern: true, ProtocolVersion: mcp.ProtocolVersion20260728})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.handleSubscriptionsListen(ctx, 7, mcp.SubscriptionsListenRequest{
			Params: mcp.SubscriptionsListenParams{Notifications: filter},
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// receivesExactly asserts that notify delivers the methods given, in order,
// every one but the first tagged with the stream's id, and nothing after them.
func receivesExactly(t *testing.T, notify <-chan mcp.JSONRPCNotification, methods ...string) {
	t.Helper()
	for i, method := range methods {
		select {
		case got := <-notify:
			require.Equal(t, method, got.Method, "notification %d", i)
			assert.Equal(t, 7, got.Params.Meta[mcp.MetaKeySubscriptionID], "notification %d", i)
		case <-time.After(2 * time.Second):
			t.Fatalf("received %d notifications, want %v", i, methods)
		}
	}
	select {
	case got := <-notify:
		t.Fatalf("received %s after %v", got.Method, methods)
	case <-time.After(100 * time.Millisecond):
	}
}

const (
	acknowledged = mcp.MethodNotificationSubscriptionsAcknowledged
	toolsChanged = mcp.MethodNotificationToolsListChanged
)

// racingStreamSession is a stream session on which the tool list changes as
// subscriptions/listen records the stream: the change is broadcast and given
// time to finish before the stream is recorded.
type racingStreamSession struct {
	*streamSession
	srv *MCPServer
}

func (s *racingStreamSession) OpenSubscriptionStream(id any, filter mcp.SubscriptionFilter) {
	broadcast := make(chan struct{})
	go func() {
		s.srv.SendNotificationToAllClients(toolsChanged, nil)
		close(broadcast)
	}()
	select {
	case <-broadcast:
	case <-time.After(100 * time.Millisecond):
	}
	s.streamSession.OpenSubscriptionStream(id, filter)
}

// A change broadcast once a stream's acknowledgement is queued reaches the
// stream, after the acknowledgement: a client that has the acknowledgement
// misses no change made after it.
func TestSubscriptionsListen_AChangeAfterTheAcknowledgementReachesTheStream(t *testing.T) {
	srv := NewMCPServer("listen-test", "1.0.0", WithToolCapabilities(true))
	session := &racingStreamSession{streamSession: newStreamSession(), srv: srv}
	openStream(t, srv, session, mcp.SubscriptionFilter{ToolsListChanged: true})
	receivesExactly(t, session.notify, acknowledged, toolsChanged)
}

// The interleavings of a broadcast with the setting up of a stream, driven by
// the test hooks: what the stream receives in each.
func TestSubscriptionsListen_ListChangesWhileTheStreamIsSetUp(t *testing.T) {
	tools := mcp.SubscriptionFilter{ToolsListChanged: true}
	newServer := func() *MCPServer {
		return NewMCPServer("listen-test", "1.0.0", WithToolCapabilities(true), WithPromptCapabilities(true))
	}

	t.Run("a broadcast between the acknowledgement and the record is sent once recorded", func(t *testing.T) {
		srv, session := newServer(), newStreamSession()
		srv.testHookListen = func(stage string) {
			if stage == "gap" {
				srv.SendNotificationToAllClients(toolsChanged, nil)
			}
		}
		openStream(t, srv, session, tools)
		receivesExactly(t, session.notify, acknowledged, toolsChanged)
	})

	t.Run("a broadcast started before the acknowledgement that reads the sessions after it", func(t *testing.T) {
		srv, session := newServer(), newStreamSession()
		started, release, delivered, finish := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		srv.testHookBroadcast = func(stage string) {
			switch stage {
			case "started":
				close(started)
				<-release
			case "delivered":
				close(delivered)
				<-finish
			}
		}
		broadcast := make(chan struct{})
		go func() {
			defer close(broadcast)
			srv.SendNotificationToAllClients(toolsChanged, nil)
		}()
		<-started
		srv.testHookListen = func(stage string) {
			switch stage {
			case "gap":
				close(release)
				<-delivered
			case "established":
				close(finish)
			}
		}
		openStream(t, srv, session, tools)
		receivesExactly(t, session.notify, acknowledged, toolsChanged)
		<-broadcast
	})

	t.Run("a broadcast that reads the sessions before the record and finishes after it", func(t *testing.T) {
		srv, session := newServer(), newStreamSession()
		delivered, finish := make(chan struct{}), make(chan struct{})
		srv.testHookBroadcast = func(stage string) {
			if stage == "delivered" {
				close(delivered)
				<-finish
			}
		}
		broadcast := make(chan struct{})
		srv.testHookListen = func(stage string) {
			switch stage {
			case "gap":
				go func() {
					defer close(broadcast)
					srv.SendNotificationToAllClients(toolsChanged, nil)
				}()
				<-delivered
			case "established":
				close(finish)
			}
		}
		openStream(t, srv, session, tools)
		receivesExactly(t, session.notify, acknowledged, toolsChanged)
		<-broadcast
	})

	t.Run("a broadcast finished before the acknowledgement is not sent", func(t *testing.T) {
		srv, session := newServer(), newStreamSession()
		srv.SendNotificationToAllClients(toolsChanged, nil)
		openStream(t, srv, session, tools)
		receivesExactly(t, session.notify, acknowledged)
	})

	t.Run("a broadcast after the record reaches the stream once", func(t *testing.T) {
		srv, session := newServer(), newStreamSession()
		established := make(chan struct{})
		srv.testHookListen = func(stage string) {
			if stage == "established" {
				close(established)
			}
		}
		openStream(t, srv, session, tools)
		<-established
		srv.SendNotificationToAllClients(toolsChanged, nil)
		receivesExactly(t, session.notify, acknowledged, toolsChanged)
	})

	t.Run("a list change the stream did not subscribe to is not sent", func(t *testing.T) {
		srv, session := newServer(), newStreamSession()
		srv.testHookListen = func(stage string) {
			if stage == "gap" {
				srv.SendNotificationToAllClients(mcp.MethodNotificationPromptsListChanged, nil)
			}
		}
		openStream(t, srv, session, tools)
		receivesExactly(t, session.notify, acknowledged)
	})
}

// Only the stdio session, which encodes every notification before any other
// code sees it, shares a stream's tag; the in-process one hands its
// notifications to user code.
func TestOnlyStdioSessionsShareStreamTags(t *testing.T) {
	_, stdio := any(NewStdioServer(NewMCPServer("t", "1")).session).(encodesNotifications)
	_, inProcess := any(NewInProcessSession("p")).(encodesNotifications)
	assert.True(t, stdio, "stdio")
	assert.False(t, inProcess, "in process")
}

package harness

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Every request is a tools/call of the echo tool with the region argument,
// modern ones carry the 2026-07-28 _meta and legacy ones none, and the region
// literal occurs once so the invalid variant replaces exactly it.
func TestTestdataShapes(t *testing.T) {
	for _, e := range eras {
		for _, p := range payloads {
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
					Meta      map[string]any `json:"_meta"`
				} `json:"params"`
			}
			body := Body(e, p)
			if err := json.Unmarshal(body, &msg); err != nil {
				t.Fatalf("%s/%s: %v", e, p, err)
			}
			if msg.Method != "tools/call" || msg.Params.Name != ToolName || msg.Params.Arguments["region"] != Region {
				t.Errorf("%s/%s: not a tools/call of %s with region %s: %.120s", e, p, ToolName, Region, body)
			}
			if hasMeta := msg.Params.Meta != nil; hasMeta != (e == Modern) {
				t.Errorf("%s/%s: _meta present = %v", e, p, hasMeta)
			}
			if n := bytes.Count(body, []byte(`"region":"`+Region+`"`)); n != 1 {
				t.Errorf("%s/%s: region literal occurs %d times", e, p, n)
			}
		}
	}
	if n := len(Schema(Complex)); n < 8192 {
		t.Errorf("complex schema is %d bytes, want at least 8192", n)
	}
}

// check refuses a response whose text is not the tool's result.
func TestCheckRefusesAWrongAnswer(t *testing.T) {
	h := http.Header{"Content-Type": {"application/json"}}
	good := []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"` + Region + `"}]}}`)
	if err := check(http.StatusOK, h, good, Small); err != nil {
		t.Fatalf("a right answer was refused: %v", err)
	}
	wrong := bytes.Replace(good, []byte(Region), []byte("eu-west-1"), 1)
	if check(http.StatusOK, h, wrong, Small) == nil {
		t.Error("a wrong text was accepted")
	}
	if check(http.StatusBadRequest, h, good, Small) == nil {
		t.Error("a 400 was accepted")
	}
	if check(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, good, Small) == nil {
		t.Error("an SSE answer was accepted")
	}
}

// The loopback leg drains every body, so the client reuses one connection
// instead of measuring a TCP handshake per call.
func TestLoopbackReusesOneConnection(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 32<<10))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	client := loopbackClient()
	defer client.CloseIdleConnections()
	for range 100 {
		if err := postLoopback(client, srv.URL+"/mcp", Body(Legacy, Small), Header(Legacy)); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("100 sequential calls opened %d connections, want 1", n)
	}
}

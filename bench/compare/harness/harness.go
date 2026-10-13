// Package harness drives one MCP library's Streamable HTTP handler through
// the scoreboard's legs. Each library's module supplies a handler; the
// requests, the checks and the timing loops live here, so every library is
// measured with the same bytes and pays the same harness cost.
package harness

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

//go:embed testdata/*.json
var testdata embed.FS

// Era is the protocol revision a request speaks.
type Era string

const (
	Modern Era = "modern" // 2026-07-28: sessionless, _meta and the standard headers
	Legacy Era = "legacy" // 2025-11-25, without a session
)

// Payload is the size of the call.
type Payload string

const (
	Small   Payload = "small"   // one-field schema, short text result
	Complex Payload = "complex" // 8 KB schema validated, nested arguments, ~16 KB result
)

var (
	eras     = []Era{Modern, Legacy}
	payloads = []Payload{Small, Complex}
)

const (
	ToolName = "echo"
	Region   = "us-east-1"
)

// NewHandler builds a library's Streamable HTTP handler serving one tool,
// ToolName, with Schema(p) as its input schema and validation on for Complex.
// The tool answers one text content: ResultText(region argument, p).
type NewHandler func(p Payload) http.Handler

// Schema is the tool's input schema for p.
func Schema(p Payload) json.RawMessage { return mustRead("schema-" + string(p) + ".json") }

// filler makes a Complex result about 16 KB.
var filler = strings.Repeat("0123456789abcdef", 1024)

// ResultText is the text the tool answers for region.
func ResultText(region string, p Payload) string {
	if p == Complex {
		return region + ":" + filler
	}
	return region
}

// Body is the tools/call request body for e and p.
func Body(e Era, p Payload) []byte { return mustRead(string(e) + "-" + string(p) + ".json") }

// Header is the request header for e: the standard 2026-07-28 headers for a
// modern request, the protocol version alone for a legacy one.
func Header(e Era) http.Header {
	h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	if e == Modern {
		h.Set("Mcp-Protocol-Version", "2026-07-28")
		h.Set("Mcp-Method", "tools/call")
		h.Set("Mcp-Name", ToolName)
	} else {
		h.Set("Mcp-Protocol-Version", "2025-11-25")
	}
	return h
}

// Verify proves every leg answers the tool's result before anything is timed,
// and that a Complex leg refuses an invalid argument, so validation is on.
func Verify(t *testing.T, newHandler NewHandler) {
	for _, p := range payloads {
		h := newHandler(p)
		for _, e := range eras {
			t.Run(string(e)+"/"+string(p), func(t *testing.T) {
				w := serve(h, Body(e, p), Header(e))
				if err := check(w.Code, w.Header(), w.Body.Bytes(), p); err != nil {
					t.Fatal(err)
				}
				t.Logf("response: %d bytes", w.Body.Len())
			})
			if p == Complex {
				t.Run(string(e)+"/complex/invalid-region", func(t *testing.T) {
					w := serve(h, invalidBody(e), Header(e))
					if err := checkRefused(w.Code, w.Body.Bytes()); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

// Bench times every leg: serial and parallel in-process for each era and
// payload, and the small payload through a loopback server for each era.
func Bench(b *testing.B, newHandler NewHandler) {
	for _, e := range eras {
		for _, p := range payloads {
			h := newHandler(p)
			body, hdr := Body(e, p), Header(e)
			if err := preflight(h, e, p); err != nil {
				b.Fatalf("not timing it: %v", err)
			}
			b.Run(string(e)+"/"+string(p)+"/serial", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					serve(h, body, hdr)
				}
			})
			b.Run(string(e)+"/"+string(p)+"/parallel", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						serve(h, body, hdr)
					}
				})
			})
		}
		b.Run(string(e)+"/small/loopback", func(b *testing.B) {
			benchLoopback(b, newHandler(Small), e)
		})
	}
}

// serve is one in-process call: its own body reader and header clone, so
// parallel calls share nothing.
func serve(h http.Handler, body []byte, hdr http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header = hdr.Clone()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// preflight proves a leg before it is timed: it answers the tool's result,
// and a Complex leg refuses an invalid argument, so its validation is on and
// it does the work the others do.
func preflight(h http.Handler, e Era, p Payload) error {
	w := serve(h, Body(e, p), Header(e))
	if err := check(w.Code, w.Header(), w.Body.Bytes(), p); err != nil {
		return fmt.Errorf("%s/%s: %w", e, p, err)
	}
	if p == Complex {
		w := serve(h, invalidBody(e), Header(e))
		if err := checkRefused(w.Code, w.Body.Bytes()); err != nil {
			return fmt.Errorf("%s/%s with an invalid region: %w", e, p, err)
		}
	}
	return nil
}

// invalidBody is the Complex request with a region of the wrong type.
func invalidBody(e Era) []byte {
	return bytes.Replace(Body(e, Complex), []byte(`"region":"`+Region+`"`), []byte(`"region":5`), 1)
}

// checkRefused accepts a tool error or a JSON-RPC error, never the result.
func checkRefused(code int, body []byte) error {
	if bytes.Contains(body, []byte(filler[:64])) {
		return fmt.Errorf("an invalid region was answered as a success: %.200s", body)
	}
	if !bytes.Contains(body, []byte(`"isError":true`)) && !bytes.Contains(body, []byte(`"error":`)) {
		return fmt.Errorf("an invalid region got neither a tool error nor a JSON-RPC error: %d %.200s", code, body)
	}
	return nil
}

// preflightLoopback proves the loopback leg before it is timed: its answer
// over the network is the tool's result, not merely a 200.
func preflightLoopback(c *http.Client, url string, e Era) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(Body(e, Small)))
	if err != nil {
		return err
	}
	req.Header = Header(e)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := check(resp.StatusCode, resp.Header, body, Small); err != nil {
		return fmt.Errorf("%s/small over the network: %w", e, err)
	}
	return nil
}

// check accepts only a 200 JSON answer carrying the tool's result text.
func check(code int, hdr http.Header, body []byte, p Payload) error {
	if code != http.StatusOK {
		return fmt.Errorf("status %d: %.200s", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("content type %q: %.200s", ct, body)
	}
	if !bytes.Contains(body, []byte(`"text":"`+ResultText(Region, p)+`"`)) {
		return fmt.Errorf("the result text is missing: %.200s", body)
	}
	return nil
}

func benchLoopback(b *testing.B, h http.Handler, e Era) {
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := loopbackClient()
	defer client.CloseIdleConnections()
	url := srv.URL + "/mcp"
	if err := preflightLoopback(client, url, e); err != nil {
		b.Fatalf("not timing it: %v", err)
	}
	body, hdr := Body(e, Small), Header(e)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := postLoopback(client, url, body, hdr); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// loopbackClient keeps enough idle connections that parallel callers never
// redial.
func loopbackClient() *http.Client {
	return &http.Client{Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024}}
}

// postLoopback is one call over the network; it drains the body so the
// connection goes back to the pool.
func postLoopback(c *http.Client, url string, body []byte, hdr http.Header) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = hdr.Clone()
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func mustRead(name string) []byte {
	data, err := testdata.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return data
}

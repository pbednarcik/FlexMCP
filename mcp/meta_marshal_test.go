package mcp

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metaMarshalJSONByMapCopy is Meta.MarshalJSON as it was before it wrote the
// object straight out: the fields and the token merged into a fresh map, then
// encoded. The new one must write the same bytes.
func metaMarshalJSONByMapCopy(m *Meta) ([]byte, error) {
	raw := make(map[string]any)
	if m.ProgressToken != nil {
		raw["progressToken"] = m.ProgressToken
	}
	maps.Copy(raw, m.AdditionalFields)
	return json.Marshal(raw)
}

func TestMetaMarshalJSONWritesWhatTheMapCopyWrote(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta Meta
		want string
	}{
		{"empty", Meta{}, `{}`},
		{"empty map", Meta{AdditionalFields: map[string]any{}}, `{}`},
		{"token only", Meta{ProgressToken: "p-1"}, `{"progressToken":"p-1"}`},
		{"integer token", Meta{ProgressToken: int64(7)}, `{"progressToken":7}`},
		{"server info", Meta{AdditionalFields: map[string]any{MetaKeyServerInfo: Implementation{Name: "bench", Version: "1.0.0"}}},
			`{"io.modelcontextprotocol/serverInfo":{"name":"bench","version":"1.0.0"}}`},
		{"token among fields", Meta{ProgressToken: "p-1", AdditionalFields: map[string]any{
			MetaKeyProtocolVersion: "2026-07-28", "a": 1, "z": true, "q": nil,
		}}, `{"a":1,"io.modelcontextprotocol/protocolVersion":"2026-07-28","progressToken":"p-1","q":null,"z":true}`},
		{"a field overrides the token", Meta{ProgressToken: "p-1", AdditionalFields: map[string]any{"progressToken": "field"}},
			`{"progressToken":"field"}`},
		{"html and unicode", Meta{AdditionalFields: map[string]any{"<k>": "a&b", "é": " ", "\U0001F600": "x", "�": 1.5}}, ""},
		{"nested", Meta{AdditionalFields: map[string]any{"n": map[string]any{"b": []any{1, "x"}, "a": map[string]any{}}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := metaMarshalJSONByMapCopy(&tc.meta)
			require.NoError(t, err)
			if tc.want != "" {
				require.Equal(t, tc.want, string(want), "the reference itself")
			}
			got, err := tc.meta.MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
			wantEnclosed, err := enclosed(&tc.meta, true)
			require.NoError(t, err)
			gotEnclosed, err := enclosed(&tc.meta, false)
			require.NoError(t, err)
			assert.Equal(t, string(wantEnclosed), string(gotEnclosed), "inside an enclosing json.Marshal")
		})
	}
}

// The new encoder and the map copy write the same bytes, or both fail, for
// any keys and values.
func FuzzMetaMarshalJSON(f *testing.F) {
	f.Add("a", "b", "progressToken", "v", 1.5, true, false)
	f.Add("<", "&", " ", "é", 0.5, false, true)
	f.Add("", "\U0001F600", "�", "\xff", 1e21, true, true)
	f.Fuzz(func(t *testing.T, k1, k2, k3, s string, n float64, token, numberToken bool) {
		fields := map[string]any{k1: s, k2: n, k3: map[string]any{s: []any{k1, n}}}
		meta := Meta{AdditionalFields: fields}
		if token {
			meta.ProgressToken = s
			if numberToken {
				meta.ProgressToken = n
			}
		}
		want, wantErr := metaMarshalJSONByMapCopy(&meta)
		got, gotErr := meta.MarshalJSON()
		if (wantErr != nil) != (gotErr != nil) {
			t.Fatalf("errors differ: map copy %v, new %v", wantErr, gotErr)
		}
		if wantErr == nil && string(want) != string(got) {
			t.Fatalf("bytes differ:\n map copy %q\n new      %q", want, got)
		}
		wantEnclosed, wantErr := enclosed(&meta, true)
		gotEnclosed, gotErr := enclosed(&meta, false)
		if (wantErr != nil) != (gotErr != nil) {
			t.Fatalf("enclosed errors differ: map copy %v, new %v", wantErr, gotErr)
		}
		if wantErr == nil && string(wantEnclosed) != string(gotEnclosed) {
			t.Fatalf("enclosed bytes differ:\n map copy %q\n new      %q", wantEnclosed, gotEnclosed)
		}
	})
}

// A result's _meta and a modern request's, inside the json.Marshal that
// carries them, cost fewer allocations than the map copy did: written into
// the enclosing encoder they need no map, no buffer and no bytes of their own.
func TestMetaMarshalJSONAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, tc := range []struct {
		name string
		meta *Meta
	}{
		{"a result's _meta", &Meta{AdditionalFields: map[string]any{MetaKeyServerInfo: Implementation{Name: "bench", Version: "1.0.0"}}}},
		{"a modern request's _meta", &Meta{ProgressToken: "p-1", AdditionalFields: map[string]any{
			MetaKeyProtocolVersion:    "2026-07-28",
			MetaKeyClientInfo:         Implementation{Name: "c", Version: "1"},
			MetaKeyClientCapabilities: ClientCapabilities{},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := testing.AllocsPerRun(1000, func() { _, _ = enclosed(tc.meta, true) })
			after := testing.AllocsPerRun(1000, func() { _, _ = enclosed(tc.meta, false) })
			t.Logf("map copy %v, written into the encoder %v", before, after)
			assert.Less(t, after, before)
		})
	}
}

// metaByMapCopy is a _meta written the way Meta was before, for comparing the
// two inside an enclosing json.Marshal, the path every result and request
// takes.
type metaByMapCopy Meta

func (m *metaByMapCopy) MarshalJSON() ([]byte, error) { return metaMarshalJSONByMapCopy((*Meta)(m)) }

// enclosed marshals a _meta the way a result carries it, by the map copy or
// by Meta itself.
func enclosed(m *Meta, byMapCopy bool) ([]byte, error) {
	if byMapCopy {
		return json.Marshal(struct {
			Meta *metaByMapCopy `json:"_meta,omitempty"`
			Text string         `json:"text"`
		}{(*metaByMapCopy)(m), "x"})
	}
	return json.Marshal(struct {
		Meta *Meta  `json:"_meta,omitempty"`
		Text string `json:"text"`
	}{m, "x"})
}

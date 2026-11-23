package mcp

import (
	"encoding/json"
	"testing"
)

// A key that only folds onto progressToken is a field of its own, never the
// token, also when the token is a number the slow path reads again to keep
// it exact.
func TestMeta_FoldedKeyIsNotTheProgressToken(t *testing.T) {
	cases := []struct {
		in    string
		token ProgressToken
		extra string
	}{
		{`{"progressToken":1,"ProgressToken":2}`, int64(1), "ProgressToken"},
		{`{"progressToken":1,"PROGRESSTOKEN":"b"}`, int64(1), "PROGRESSTOKEN"},
		{`{"progressToken":1,"progreſſToken":2}`, int64(1), "progreſſToken"},
		{`{"progressToken":1.5,"ProgressToken":2}`, 1.5, "ProgressToken"},
		{`{"progressToken":12345678901234567890,"ProgressToken":1}`, json.Number("12345678901234567890"), "ProgressToken"},
		{`{"ProgressToken":2,"progressToken":1}`, int64(1), "ProgressToken"},
	}
	for _, c := range cases {
		var m Meta
		if err := json.Unmarshal([]byte(c.in), &m); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if m.ProgressToken != c.token {
			t.Errorf("%s: token %#v, want %#v", c.in, m.ProgressToken, c.token)
		}
		if _, ok := m.AdditionalFields[c.extra]; !ok || len(m.AdditionalFields) != 1 {
			t.Errorf("%s: additional fields %v, want only %q", c.in, m.AdditionalFields, c.extra)
		}
	}
}

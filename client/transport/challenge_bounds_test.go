package transport

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A header's parameters belong to a challenge of that header: a parameter
// on a line that starts no challenge is dropped, not attached to the last
// challenge of the line before.
func TestParseChallengesKeepsEachHeaderLineToItself(t *testing.T) {
	challenges := parseChallenges([]string{`Bearer realm="a"`, `scope="x y"`})
	require.Len(t, challenges, 1)
	assert.Equal(t, "a", challenges[0].params["realm"])
	assert.NotContains(t, challenges[0].params, "scope")
}

// A repeated parameter keeps its first value (RFC 6750 section 3 forbids
// a repeat; the first is the one read as the header stands).
func TestParseChallengesKeepsTheFirstOfARepeatedParameter(t *testing.T) {
	challenges := parseChallenges([]string{`Bearer error="invalid_token", error="insufficient_scope"`})
	require.Len(t, challenges, 1)
	assert.Equal(t, "invalid_token", challenges[0].params["error"])
}

// A parameter name compares case-insensitively (RFC 9110 section 11.2), so
// a repeat in another letter case is the same name.
func TestParseChallengesComparesParameterNamesCaseInsensitively(t *testing.T) {
	challenges := parseChallenges([]string{`Bearer Error="invalid_token", error="insufficient_scope"`})
	require.Len(t, challenges, 1)
	assert.Equal(t, "invalid_token", challenges[0].params["error"])
}

// A header of many bare tokens is cut at the challenge bound: the memory a
// hostile server can make the parser allocate is a small constant, whatever
// the header's size.
func TestParseChallengesIsBoundedInChallengesAndAllocations(t *testing.T) {
	header := strings.Repeat("a,", 1<<20)
	challenges := parseChallenges([]string{header})
	assert.LessOrEqual(t, len(challenges), maxChallenges)

	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	allocs := testing.AllocsPerRun(5, func() { parseChallenges([]string{header}) })
	assert.LessOrEqual(t, allocs, 64.0, "allocations for a 2 MiB header of bare tokens")
}

// A challenge keeps at most maxParams parameters, and a parameter that is
// not kept costs nothing: a flood of names, of quoted values, or of either
// on a line that starts no challenge, allocates a small constant.
func TestParseChallengesIsBoundedInParameters(t *testing.T) {
	var names strings.Builder
	for i := 0; names.Len() < 1<<20; i++ {
		fmt.Fprintf(&names, "p%d=v,", i)
	}
	for name, headers := range map[string][]string{
		"many names":                      {"Bearer " + names.String()},
		"many quoted values":              {"Bearer " + strings.Repeat(`a="x",`, 1<<17)},
		"many escaped values":             {"Bearer " + strings.Repeat(`a="\x",`, 1<<17)},
		"a line that starts no challenge": {`Bearer realm="a"`, strings.Repeat(`a="x",`, 1<<17)},
	} {
		t.Run(name, func(t *testing.T) {
			for _, c := range parseChallenges(headers) {
				assert.LessOrEqual(t, len(c.params), maxParams)
			}
			if raceEnabled {
				t.Skip("the race detector allocates")
			}
			allocs := testing.AllocsPerRun(5, func() { parseChallenges(headers) })
			assert.LessOrEqual(t, allocs, 64.0, "allocations for the header")
		})
	}
}

// Parsing is linear in the header: a scheme followed by a long run that is
// no token68 is read once, not once per byte.
func TestParseChallengesTimeIsLinear(t *testing.T) {
	header := "Bearer " + strings.Repeat("/", 1<<20) + "!"
	done := make(chan struct{})
	go func() {
		defer close(done)
		parseChallenges([]string{header})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a 1 MiB header took more than 5 s to parse")
	}
}

func FuzzParseChallenges(f *testing.F) {
	f.Add(`Bearer realm="x", scope="a b", error="insufficient_scope"`)
	f.Add(`Basic realm="a", Bearer abc==, DPoP algs="ES256"`)
	f.Add(strings.Repeat("a,", 100))
	f.Add(`Bearer realm="unterminated`)
	f.Add("Bearer " + strings.Repeat("/", 100) + "!")
	f.Fuzz(func(t *testing.T, header string) {
		challenges := parseChallenges([]string{header})
		if len(challenges) > maxChallenges {
			t.Fatalf("%d challenges, the bound is %d", len(challenges), maxChallenges)
		}
		for _, c := range challenges {
			if len(c.params) > maxParams {
				t.Fatalf("%d parameters, the bound is %d", len(c.params), maxParams)
			}
		}
	})
}

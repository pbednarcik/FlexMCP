package mcp

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A reserve found full takes decoders back again once one was taken from it,
// so the decoders a request takes after collections are the warm ones, even
// after a burst filled the reserve and the collector emptied the pool.
func TestDecoderReserveRefillsOnceTakenFrom(t *testing.T) {
	var p decoderPool
	slots := len(p.slots())
	made := make(map[*valueDecoder]bool)
	var held []*valueDecoder
	for range slots + 1 {
		d := p.get()
		made[d] = true
		held = append(held, d)
	}
	require.Len(t, made, slots+1, "a decoder handed out twice")
	for _, d := range held {
		p.put(d)
	}
	for round := range 2 {
		runtime.GC()
		runtime.GC()
		held = held[:0]
		for range slots {
			d := p.get()
			require.True(t, made[d], "round %d: a new decoder, not a reserved one", round)
			held = append(held, d)
		}
		for _, d := range held {
			p.put(d)
		}
	}
	assert.True(t, made[p.get()], "a new decoder, not a reserved one")
}

// A put that finds every slot taken flags the reserve full. Should requests
// take every slot between that look and the flag, the reserve is flagged full
// and empty, and no request takes from it to clear the flag: the next request
// that finds it empty clears it instead, so the reserve refills rather than
// stay shut for the life of the process.
func TestDecoderReserveReopensWhenFoundEmpty(t *testing.T) {
	var p decoderPool
	n := len(p.slots())
	for range n {
		p.put(newValueDecoder().(*valueDecoder))
	}
	p.foundFull = func() {
		p.foundFull = nil
		for range n {
			_ = p.get()
		}
	}
	p.put(newValueDecoder().(*valueDecoder))
	require.True(t, p.full.Load(), "the reserve flagged full and emptied")

	held := make([]*valueDecoder, 0, n)
	for range n {
		held = append(held, p.get())
	}
	for _, d := range held {
		p.put(d)
	}
	kept := 0
	for i := range p.reserve {
		if p.reserve[i].d.Load() != nil {
			kept++
		}
	}
	assert.Equal(t, n, kept, "decoders the reserve keeps once it was found empty")
}

// A full reserve holds, per decoder, its string cache and a few small
// buffers, whatever the decoders read: an escape buffer a long escaped string
// grew is let go when the decode ends. 91 KiB a decoder when it was kept,
// after a 64 KiB argument of escapes; 27-28 KiB now after that or a client's
// _meta, 51 KiB with the string cache full and an escape buffer kept.
func TestReservedDecodersRetention(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	// pairs is an object of n names and n values, all different strings of
	// maxKeptString bytes.
	pairs := func(n int) []byte {
		var b strings.Builder
		b.WriteString("{")
		for i := range n {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `"k%063d":"v%063d"`, i, i)
		}
		b.WriteString("}")
		return []byte(b.String())
	}
	// fullAndEscaped is an array of maxKeptStrings different values of
	// maxKeptString bytes, each kept and boxed, and one escaped string whose
	// escape buffer stays under maxKeptUnquoted, so it is kept too.
	fullAndEscaped := func() []byte {
		var b strings.Builder
		b.WriteString("[")
		for i := range maxKeptStrings {
			fmt.Fprintf(&b, `"%0*d",`, maxKeptString, i)
		}
		b.WriteString(`"`)
		b.WriteString(strings.Repeat(`\n`, maxKeptUnquoted*15/32))
		b.WriteString(`"]`)
		return []byte(b.String())
	}()
	d := newValueDecoder().(*valueDecoder)
	_, _ = d.decodeArguments(fullAndEscaped)
	require.Len(t, d.strs, maxKeptStrings, "strings the decoder keeps")
	require.NotZero(t, cap(d.unq), "the escape buffer the decoder keeps")
	inputs := map[string][]byte{
		"a full string cache and an escape buffer": fullAndEscaped,
		"a _meta":              []byte(metaClaudeCode),
		"64 KiB of escapes":    []byte(`{"s":"` + strings.Repeat(`\n`, (maxFastInput-16)/2) + `"}`),
		"nested 10,000 deep":   []byte(strings.Repeat("[", 10000) + strings.Repeat("]", 10000)),
		"a full string cache":  pairs(maxKeptStrings / 2),
		"300 names and values": pairs(300),
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			var p decoderPool
			n := len(p.slots())
			before := heap()
			held := make([]*valueDecoder, n)
			for i := range held {
				held[i] = p.get()
				_, _ = held[i].decodeArguments(input)
			}
			for _, d := range held {
				p.put(d)
			}
			clear(held)
			retained := (heap() - before) / uint64(n)
			runtime.KeepAlive(&p)
			t.Logf("%d B a decoder", retained)
			assert.Less(t, retained, uint64(54<<10), "bytes a reserved decoder holds")
		})
	}
}

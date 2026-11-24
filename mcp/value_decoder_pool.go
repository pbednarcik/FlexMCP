package mcp

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// decoderPool keeps value decoders between requests. A sync.Pool serves
// them, and behind it a reserve of one decoder for each P that the
// collector does not empty. Without it, each request that finds the pool
// emptied by a collection makes a decoder of its own, with a string cache
// sized for maxKeptStrings and none of the strings a client repeats, and a
// server on many cores collects often enough that most requests would. The
// reserve takes no lock, which a reserve under a mutex cost on the parallel
// legs: each of its slots holds one decoder or none. Decoders go back to it
// until it is found full, and again once a request took one from it; while
// it is full, to the pool, after one atomic load; a request that finds the
// reserve empty clears the flag too, so a reserve emptied between a put's
// look and its flag reopens. A full reserve keeps its decoders for the life
// of the process: 27-28 KiB each after the decodes clients send, at most
// 45 KiB whatever they read, its string cache full (maxKeptStrings of
// maxKeptString bytes; an escape buffer past maxKeptUnquoted is not kept).
// At 36 Ps the three pools' reserves hold about 2.9 MiB, 4.8 MiB at most.
type decoderPool struct {
	pool sync.Pool
	// numbers marks the decoders the pool makes as keeping numbers as the
	// json.Number of their literal.
	numbers bool
	// full says the reserve was found full, and no decoder was taken from
	// it since.
	full    atomic.Bool
	reserve []reserveSlot
	once    sync.Once
	// foundFull, when not nil, runs between a put's look at a full reserve
	// and its flag: a test's schedule. It is nil in every pool but a test's.
	foundFull func()
}

// reserveSlot holds one reserved decoder, or none, on a 128-byte line of its
// own, the cache line of Apple silicon and twice that of most x86 and Arm
// cores: requests on different cores take and give back decoders without
// contending for one line.
type reserveSlot struct {
	d atomic.Pointer[valueDecoder]
	_ [120]byte
}

// slots returns the reserve, made on first use with one slot for each P
// GOMAXPROCS names then, the most that decode at once; a later change of
// GOMAXPROCS leaves it as made.
func (p *decoderPool) slots() []reserveSlot {
	p.once.Do(func() { p.reserve = make([]reserveSlot, runtime.GOMAXPROCS(0)) })
	return p.reserve
}

// get returns a decoder from the pool, else from the reserve, else a new one.
// The reserve is looked through from its first slot, as put fills it, so the
// decoder given back last is taken first, its cache warm with the strings
// this server's clients send.
func (p *decoderPool) get() *valueDecoder {
	if d, ok := p.pool.Get().(*valueDecoder); ok {
		return d
	}
	for i := range p.slots() {
		slot := &p.reserve[i]
		if slot.d.Load() == nil {
			continue
		}
		if d := slot.d.Swap(nil); d != nil {
			if p.full.Load() {
				p.full.Store(false)
			}
			return d
		}
	}
	// The reserve is empty: one flagged full stays so only until a request
	// takes from it, so the flag is cleared here as well.
	if p.full.Load() {
		p.full.Store(false)
	}
	d := newValueDecoder().(*valueDecoder)
	d.numbers = p.numbers
	return d
}

// put gives d back: to the reserve while it has room, else to the pool.
func (p *decoderPool) put(d *valueDecoder) {
	if !p.full.Load() {
		for i := range p.slots() {
			slot := &p.reserve[i]
			if slot.d.Load() == nil && slot.d.CompareAndSwap(nil, d) {
				return
			}
		}
		if p.foundFull != nil {
			p.foundFull()
		}
		p.full.Store(true)
	}
	p.pool.Put(d)
}

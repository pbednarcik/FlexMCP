package server

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/pbednarcik/flexmcp/mcp"
)

// JSON-RPC member names are spelled one way, but encoding/json matches a
// member to a field under strings.EqualFold, so "Method", "METHOD" and
// "methoſ" (U+017F) all read as method, and of a member named twice it keeps
// the last. checkMemberNames finds a message that reads as a request only
// that way.

// envelopeMembers are the members of a JSON-RPC message; a member's bit in
// memberCheck is 1 << its index.
var envelopeMembers = memberNames("jsonrpc", "method", "id", "params", "result", "error")

const (
	memberMethod = 1 << (iota + 1)
	memberID
	memberParams
	memberResult
	memberError
)

// paramsMembers are the params members the server decodes into fields: those
// of every request dispatchRequest decodes, and those of paramsPeek, which
// holds the one notifications/cancelled names. What their values hold, tool
// and prompt arguments above all, is not looked into.
var paramsMembers = memberNames(
	"_meta", "name", "arguments", "uri", "cursor", "taskId", "requestId",
	"inputResponses", "requestState", "task", "level", "ref", "argument",
	"context", "notifications", "protocolVersion", "capabilities", "clientInfo",
)

func memberNames(names ...string) [][]byte {
	members := make([][]byte, len(names))
	for i, name := range names {
		members[i] = []byte(name)
	}
	return members
}

// memberCheck is what checkMemberNames found in a message.
type memberCheck struct {
	// refused is the first member refused, if any.
	refused memberRefusal
	// exact and folded hold the bit of each envelope member sent spelled as
	// defined, and sent spelled some other way that folds onto it.
	exact, folded uint8
	// id is the value of the id member spelled as defined, when there is one
	// and only one.
	id json.RawMessage
}

// memberRefusal is a member refused: its name as sent, nil for none, and
// the name it folds onto, nil when it is a defined member sent twice.
type memberRefusal struct {
	name, defined []byte
	params        bool
}

// refuse records name unless a member was refused already.
func (r *memberRefusal) refuse(name, defined []byte, params bool) {
	if r.name == nil {
		*r = memberRefusal{name: name, defined: defined, params: params}
	}
}

// err is the refusal as an error; nil when no member was refused.
func (c memberCheck) err() error {
	r := c.refused
	if r.name == nil {
		return nil
	}
	path := ""
	if r.params {
		path = "params."
	}
	if r.defined == nil {
		return fmt.Errorf("duplicate member %q", path+string(r.name))
	}
	return fmt.Errorf("member %q is not the defined name %q", path+string(r.name), path+string(r.defined))
}

// checkMemberNames reports the first member of message, a JSON object
// encoding/json read already, whose name differs from an envelope member's
// only by case or Unicode folding, or that names an envelope member twice.
// The params of a request, or of the cancellation method, are checked the
// same way against paramsMembers; a notification's are its handler's own.
// method is the message's method as decoded. Nothing is decoded and, for a
// message that passes, nothing is allocated.
func checkMemberNames(message []byte, method mcp.MCPMethod) (c memberCheck) {
	var params memberRefusal
	members := objectMembers(message)
	for {
		name, ok := members.name()
		if !ok {
			break
		}
		name = memberName(name)
		k, exact := matchMember(name, envelopeMembers)
		if k < 0 {
			members.skipValue()
			continue
		}
		bit := uint8(1) << k
		switch {
		case !exact:
			c.folded |= bit
			c.refused.refuse(name, envelopeMembers[k], false)
			members.skipValue()
		case c.exact&bit != 0:
			c.refused.refuse(name, nil, false)
			if bit == memberID {
				c.id = nil
			}
			members.skipValue()
		case bit == memberParams:
			// params is walked once, for its end and its members alike.
			c.exact |= bit
			params = members.checkParams()
		default:
			c.exact |= bit
			value := members.skipValue()
			if bit == memberID {
				c.id = value
			}
		}
	}
	if c.exact&memberID != 0 || method == mcp.MethodNotificationCancelled {
		c.refused.refuse(params.name, params.defined, true)
	}
	return c
}

// checkParams walks the params value the walk stands at, and past it: the
// first of its members whose name folds onto a member of paramsMembers or
// names one twice is refused.
func (m *jsonMembers) checkParams() (refused memberRefusal) {
	params := jsonMembers{b: m.b, i: m.i}
	if !params.enter() {
		m.skipValue()
		return refused
	}
	var seen uint32
	for {
		name, ok := params.name()
		if !ok {
			break
		}
		if refused.name == nil {
			name = memberName(name)
			if k, exact := matchMember(name, paramsMembers); k >= 0 {
				bit := uint32(1) << k
				switch {
				case !exact:
					refused.refuse(name, paramsMembers[k], true)
				case seen&bit != 0:
					refused.refuse(name, nil, true)
				}
				seen |= bit
			}
		}
		params.skipValue()
	}
	m.resume(min(params.i+1, len(m.b)))
	return refused
}

// unanswered reports whether the message, read by its members' names in any
// spelling, is a notification, a method without an id, or a response, an id
// with a result or an error and no method: neither is ever answered.
func (c memberCheck) unanswered() bool {
	sent := c.exact | c.folded
	if sent&memberID == 0 {
		return sent&memberMethod != 0
	}
	return sent&memberMethod == 0 && sent&(memberResult|memberError) != 0
}

// matchMember is the index of the member of names that name is, or folds onto
// under strings.EqualFold, and whether name is spelled as that member; -1
// when it is neither.
func matchMember(name []byte, names [][]byte) (k int, exact bool) {
	for i, defined := range names {
		if bytes.Equal(name, defined) {
			return i, true
		}
	}
	for i, defined := range names {
		// A name folds onto an ASCII one only at its length or, through
		// letters such as U+017F, longer.
		if len(name) >= len(defined) && bytes.EqualFold(name, defined) {
			return i, false
		}
	}
	return -1, false
}

// memberName is the name a quoted member name stands for. A name with an
// escape is decoded, at the cost of an allocation.
func memberName(quoted []byte) []byte {
	name := quoted[1 : len(quoted)-1]
	if bytes.IndexByte(name, '\\') < 0 {
		return name
	}
	var decoded string
	if json.Unmarshal(quoted, &decoded) != nil {
		return name
	}
	return []byte(decoded)
}

// jsonMembers walks the members of a JSON object. The text is JSON already,
// so the walk only finds where each name and value lies; on text that is not,
// it stops early or runs to the end, never past it.
type jsonMembers struct {
	b []byte
	i int
}

// objectMembers walks the members of the object b holds; of a value that is
// no object, none.
func objectMembers(b []byte) jsonMembers {
	m := jsonMembers{b: b}
	if !m.enter() {
		return jsonMembers{}
	}
	return m
}

// enter steps into the object the walk stands at; false when it stands at
// no object.
func (m *jsonMembers) enter() bool {
	i := skipJSONSpace(m.b, m.i)
	if i >= len(m.b) || m.b[i] != '{' {
		return false
	}
	m.i = i + 1
	return true
}

// name is the next member's name, quotes included, and leaves the walk at
// its value; ok is false past the last member, where the walk stands at the
// object's closing brace.
func (m *jsonMembers) name() (name []byte, ok bool) {
	b := m.b
	i := skipJSONSpace(b, m.i)
	m.i = i
	if i >= len(b) || b[i] != '"' {
		return nil, false
	}
	end := skipJSONString(b, i)
	name = b[i:end]
	i = skipJSONSpace(b, end)
	if i >= len(b) || b[i] != ':' {
		m.i = len(b)
		return nil, false
	}
	m.i = skipJSONSpace(b, i+1)
	return name, true
}

// skipValue passes over the value the walk stands at, and returns it.
func (m *jsonMembers) skipValue() []byte {
	start := m.i
	end := skipJSONValue(m.b, start)
	m.resume(end)
	return m.b[start:end]
}

// resume continues the walk at b[end], past a value.
func (m *jsonMembers) resume(end int) {
	i := skipJSONSpace(m.b, end)
	if i < len(m.b) && m.b[i] == ',' {
		i++
	}
	m.i = i
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipJSONString is the index past the string that starts at b[i].
func skipJSONString(b []byte, i int) int {
	start := i
	for i++; i < len(b); {
		j := bytes.IndexByte(b[i:], '"')
		if j < 0 {
			return len(b)
		}
		i += j + 1
		// The quote ends the string unless an odd run of backslashes
		// escapes it.
		k := i - 2
		for k > start && b[k] == '\\' {
			k--
		}
		if (i-2-k)%2 == 0 {
			return i
		}
	}
	return len(b)
}

// skipJSONValue is the index past the value that starts at b[i].
func skipJSONValue(b []byte, i int) int {
	if i >= len(b) {
		return i
	}
	switch b[i] {
	case '"':
		return skipJSONString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipJSONString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' &&
		b[i] != ' ' && b[i] != '\t' && b[i] != '\n' && b[i] != '\r' {
		i++
	}
	return i
}

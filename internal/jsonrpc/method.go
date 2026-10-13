// Package jsonrpc holds the JSON-RPC 2.0 message rules the server and the
// client transports share.
package jsonrpc

import (
	"encoding/json"
	"unicode/utf8"
)

// Method is a message's method member as decoded. JSON-RPC 2.0 (sections 4
// and 5) tells a request from a response by the member being there, not by
// its value: "method":"" asks for a method named "", and a method that is
// not a string makes an invalid request, never a response.
type Method struct {
	// Present says the message has a method member, whatever its value.
	Present bool
	// IsString says that member is a string, so Name is its value.
	IsString bool
	Name     string
}

// UnmarshalJSON records the member's presence and, for a string, its value.
// A string without escapes or non-ASCII bytes is taken as it stands.
func (m *Method) UnmarshalJSON(data []byte) error {
	*m = Method{Present: true}
	if len(data) < 2 || data[0] != '"' {
		return nil
	}
	if plainString(data[1 : len(data)-1]) {
		m.Name = string(data[1 : len(data)-1])
	} else if err := json.Unmarshal(data, &m.Name); err != nil {
		return err
	}
	m.IsString = true
	return nil
}

// MethodOf is the method member of message, whose decode into a string field
// gave name. A name settles it at no cost; an empty one is a method named "",
// a null or no member at all, which only a second look at message tells
// apart. A server, whose messages nearly all carry a name, takes this path;
// decoding a Method costs an allocation per message, a name nothing.
func MethodOf(name string, message []byte) Method {
	if name != "" {
		return Method{Present: true, IsString: true, Name: name}
	}
	var envelope struct {
		Method Method `json:"method"`
	}
	if json.Unmarshal(message, &envelope) != nil {
		return Method{}
	}
	return envelope.Method
}

// plainString reports whether the inside of a JSON string needs no decoding.
func plainString(s []byte) bool {
	for _, c := range s {
		if c == '\\' || c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

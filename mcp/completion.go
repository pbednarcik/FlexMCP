package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
)

/* Autocomplete */

// CompleteRequest is a request from the client to the server, to ask for completion options.
type CompleteRequest struct {
	Request
	Params CompleteParams `json:"params"`
	Header http.Header    `json:"-"`
}

// CompleteParams are the parameters for a completion/complete request
type CompleteParams struct {
	Ref      any              `json:"ref"` // Can be PromptReference or ResourceReference
	Argument CompleteArgument `json:"argument"`
	Context  CompleteContext  `json:"context"`
}

func (p *CompleteParams) UnmarshalJSON(data []byte) error {
	// Use a temporary type to avoid infinite recursion on UnmarshalJSON
	type Alias CompleteParams
	aux := &struct {
		// Use RawMessage to delay unmarshalling until after the type is known
		Ref json.RawMessage `json:"ref"`
		*Alias
	}{
		Alias: (*Alias)(p),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	// Use a temporary "type peek" struct to determine the type
	var typePeek struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(aux.Ref, &typePeek); err != nil {
		return err
	}
	switch typePeek.Type {
	case "ref/prompt":
		var prompt PromptReference
		if err := json.Unmarshal(aux.Ref, &prompt); err != nil {
			return err
		}
		p.Ref = prompt
	case "ref/resource":
		var resource ResourceReference
		if err := json.Unmarshal(aux.Ref, &resource); err != nil {
			return err
		}
		p.Ref = resource
	default:
		return fmt.Errorf("unknown reference type: %s", typePeek.Type)
	}
	return nil
}

// CompleteResult is the server's response to a completion/complete request
type CompleteResult struct {
	Result
	Completion Completion `json:"completion"`
}

// CompleteArgument is an argument to a completion request
type CompleteArgument struct {
	// The name of the argument
	Name string `json:"name"`
	// The value of the argument to use for completion matching.
	Value string `json:"value"`
}

// CompleteContext is the context about already-resolved arguments
type CompleteContext struct {
	Arguments map[string]string `json:"arguments,omitempty"`
}

// Completion is the server's response to a completion/complete request
type Completion struct {
	// An array of completion values. Must not exceed 100 items.
	Values []string `json:"values"`
	// The total number of completion options available. This can exceed the
	// number of values actually sent in the response.
	Total int `json:"total,omitempty"`
	// Indicates whether there are additional completion options beyond those
	// provided in the current response, even if the exact total is unknown.
	HasMore bool `json:"hasMore,omitempty"`
}

// ResourceReference is a reference to a resource or resource template definition.
type ResourceReference struct {
	Type string `json:"type"`
	// The URI or URI template of the resource.
	URI string `json:"uri"`
}

// PromptReference identifies a prompt.
type PromptReference struct {
	Type string `json:"type"`
	// The name of the prompt or prompt template
	Name string `json:"name"`
}

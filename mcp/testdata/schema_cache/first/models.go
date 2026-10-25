// Package models holds a tool input whose package shares its name with
// another one, for the schema cache key tests.
package models

// Request is a tool input from the first package named models.
type Request struct {
	Query string `json:"query"`
}

// Package models holds a tool input whose package shares its name with
// another one, for the schema cache key tests.
package models

// Request is a different tool input from another package named models.
type Request struct {
	Limit int `json:"limit"`
}

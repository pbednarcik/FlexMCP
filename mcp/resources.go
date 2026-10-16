package mcp

import (
	"net/http"
	"time"

	"github.com/yosida95/uritemplate/v3"
)

// ResourceOption is a function that configures a Resource.
// It provides a flexible way to set various properties of a Resource using the functional options pattern.
type ResourceOption func(*Resource)

// NewResource creates a new Resource with the given URI, name and options.
// The resource will be configured based on the provided options.
// Options are applied in order, allowing for flexible resource configuration.
func NewResource(uri string, name string, opts ...ResourceOption) Resource {
	resource := Resource{
		URI:  uri,
		Name: name,
	}

	for _, opt := range opts {
		opt(&resource)
	}

	return resource
}

// WithResourceDescription adds a description to the Resource.
// The description should provide a clear, human-readable explanation of what the resource represents.
func WithResourceDescription(description string) ResourceOption {
	return func(r *Resource) {
		r.Description = description
	}
}

// WithResourceTitle sets the optional human-readable display title for the Resource.
// Per the MCP spec, clients should prefer Title over Name for display.
func WithResourceTitle(title string) ResourceOption {
	return func(r *Resource) {
		r.Title = title
	}
}

// WithResourceSize sets the size of the raw resource content in bytes.
// This is the size before base64 encoding or any tokenization, and is used
// by hosts to display file sizes and estimate context window usage.
// Negative values are ignored, since the MCP schema defines size as a byte
// count which is necessarily non-negative.
func WithResourceSize(size int64) ResourceOption {
	return func(r *Resource) {
		if size < 0 {
			return
		}
		r.Size = &size
	}
}

// WithMIMEType sets the MIME type for the Resource.
// This should indicate the format of the resource's contents.
func WithMIMEType(mimeType string) ResourceOption {
	return func(r *Resource) {
		r.MIMEType = mimeType
	}
}

// WithAnnotations returns a ResourceOption that sets the resource's Annotations fields.
// It initializes Annotations if nil, sets Audience to the provided slice,
// stores Priority as a pointer to the provided value, and sets LastModified to the provided timestamp.
func WithAnnotations(audience []Role, priority float64, lastModified string) ResourceOption {
	return func(r *Resource) {
		if r.Annotations == nil {
			r.Annotations = &Annotations{}
		}
		r.Annotations.Audience = audience
		r.Annotations.Priority = &priority
		r.Annotations.LastModified = lastModified
	}
}

// WithLastModified returns a ResourceOption that sets the resource's Annotations.LastModified
// to the provided timestamp. If the resource's Annotations is nil, it will be initialized.
// The timestamp is expected to be an ISO 8601 (RFC3339) formatted string (e.g., "2025-01-12T15:00:58Z").
func WithLastModified(timestamp string) ResourceOption {
	return func(r *Resource) {
		if r.Annotations == nil {
			r.Annotations = &Annotations{}
		}
		r.Annotations.LastModified = timestamp
	}
}

// ResourceTemplateOption is a function that configures a ResourceTemplate.
// It provides a flexible way to set various properties of a ResourceTemplate using the functional options pattern.
type ResourceTemplateOption func(*ResourceTemplate)

// NewResourceTemplate creates a new ResourceTemplate with the given URI template, name and options.
// The template will be configured based on the provided options.
// Options are applied in order, allowing for flexible template configuration.
func NewResourceTemplate(uriTemplate string, name string, opts ...ResourceTemplateOption) ResourceTemplate {
	template := ResourceTemplate{
		URITemplate: &URITemplate{Template: uritemplate.MustNew(uriTemplate)},
		Name:        name,
	}

	for _, opt := range opts {
		opt(&template)
	}

	return template
}

// WithTemplateDescription adds a description to the ResourceTemplate.
// The description should provide a clear, human-readable explanation of what resources this template represents.
func WithTemplateDescription(description string) ResourceTemplateOption {
	return func(t *ResourceTemplate) {
		t.Description = description
	}
}

// WithTemplateTitle sets the optional human-readable display title for the ResourceTemplate.
// Per the MCP spec, clients should prefer Title over Name for display.
func WithTemplateTitle(title string) ResourceTemplateOption {
	return func(t *ResourceTemplate) {
		t.Title = title
	}
}

// WithTemplateMIMEType sets the MIME type for the ResourceTemplate.
// This should only be set if all resources matching this template will have the same type.
func WithTemplateMIMEType(mimeType string) ResourceTemplateOption {
	return func(t *ResourceTemplate) {
		t.MIMEType = mimeType
	}
}

// WithTemplateAnnotations returns a ResourceTemplateOption that sets the template's
// Annotations field, initializing it if nil, and setting Audience, Priority, and LastModified.
func WithTemplateAnnotations(audience []Role, priority float64, lastModified string) ResourceTemplateOption {
	return func(t *ResourceTemplate) {
		if t.Annotations == nil {
			t.Annotations = &Annotations{}
		}
		t.Annotations.Audience = audience
		t.Annotations.Priority = &priority
		t.Annotations.LastModified = lastModified
	}
}

// ValidateISO8601Timestamp verifies that timestamp is a valid ISO 8601 timestamp
// using the RFC3339 layout. An empty string is considered valid. It returns nil
// when the timestamp is valid, or the parsing error when it is not.
func ValidateISO8601Timestamp(timestamp string) error {
	if timestamp == "" {
		return nil // Empty is valid (optional field)
	}
	// Use time.RFC3339 for ISO 8601 compatibility
	_, err := time.Parse(time.RFC3339, timestamp)
	return err
}

// WithResourceIcons adds icons to the Resource.
// Icons provide visual identifiers for the resource.
func WithResourceIcons(icons ...Icon) ResourceOption {
	return func(r *Resource) {
		r.Icons = icons
	}
}

// WithTemplateIcons adds icons to the ResourceTemplate.
// Icons provide visual identifiers for the resource template.
func WithTemplateIcons(icons ...Icon) ResourceTemplateOption {
	return func(rt *ResourceTemplate) {
		rt.Icons = icons
	}
}

/* Resources */

// ListResourcesRequest is sent from the client to request a list of resources
// the server has.
type ListResourcesRequest struct {
	PaginatedRequest
	Header http.Header `json:"-"`
}

// ListResourcesResult is the server's response to a resources/list request
// from the client.
type ListResourcesResult struct {
	PaginatedResult
	Resources []Resource `json:"resources"`
}

// ListResourceTemplatesRequest is sent from the client to request a list of
// resource templates the server has.
type ListResourceTemplatesRequest struct {
	PaginatedRequest
	Header http.Header `json:"-"`
}

// ListResourceTemplatesResult is the server's response to a
// resources/templates/list request from the client.
type ListResourceTemplatesResult struct {
	PaginatedResult
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
}

// ReadResourceRequest is sent from the client to the server, to read a
// specific resource URI.
type ReadResourceRequest struct {
	Request
	Header http.Header        `json:"-"`
	Params ReadResourceParams `json:"params"`
}

type ReadResourceParams struct {
	// The URI of the resource to read. The URI can use any protocol; it is up
	// to the server how to interpret it.
	URI string `json:"uri"`
	// Arguments to pass to the resource handler
	Arguments map[string]any `json:"arguments,omitempty"`
	// Meta carries protocol-level metadata (e.g. W3C traceparent, progressToken).
	Meta *Meta `json:"_meta,omitempty"`
	MultiRoundTripParams
}

// ReadResourceResult is the server's response to a resources/read request
// from the client.
type ReadResourceResult struct {
	CacheableResult
	MultiRoundTripResult
	Contents []ResourceContents `json:"contents"` // Can be TextResourceContents or BlobResourceContents
}

// ResourceListChangedNotification is an optional notification from the server
// to the client, informing it that the list of resources it can read from has
// changed. This may be issued by servers without any previous subscription from
// the client.
type ResourceListChangedNotification struct {
	Notification
}

// SubscribeRequest is sent from the client to request resources/updated
// notifications from the server whenever a particular resource changes.
type SubscribeRequest struct {
	Request
	Params SubscribeParams `json:"params"`
	Header http.Header     `json:"-"`
}

type SubscribeParams struct {
	// The URI of the resource to subscribe to. The URI can use any protocol; it
	// is up to the server how to interpret it.
	URI string `json:"uri"`
}

// UnsubscribeRequest is sent from the client to request cancellation of
// resources/updated notifications from the server. This should follow a previous
// resources/subscribe request.
type UnsubscribeRequest struct {
	Request
	Params UnsubscribeParams `json:"params"`
	Header http.Header       `json:"-"`
}

type UnsubscribeParams struct {
	// The URI of the resource to unsubscribe from.
	URI string `json:"uri"`
}

// ResourceUpdatedNotification is a notification from the server to the client,
// informing it that a resource has changed and may need to be read again. It is
// only sent for a URI the client listed in the resourceSubscriptions of its
// subscriptions/listen request.
type ResourceUpdatedNotification struct {
	Notification
	Params ResourceUpdatedNotificationParams `json:"params"`
}
type ResourceUpdatedNotificationParams struct {
	// The URI of the resource that has been updated. This might be a sub-
	// resource of the one that the client actually subscribed to.
	URI string `json:"uri"`
}

// Resource represents a known resource that the server is capable of reading.
type Resource struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta `json:"_meta,omitempty"`
	// The URI of this resource.
	URI string `json:"uri"`
	// A human-readable name for this resource.
	//
	// This can be used by clients to populate UI elements.
	Name string `json:"name"`
	// Title is an optional human-readable, UI-friendly display name for this resource.
	// If not provided, clients should fall back to Name.
	Title string `json:"title,omitempty"`
	// A description of what this resource represents.
	//
	// This can be used by clients to improve the LLM's understanding of
	// available resources. It can be thought of like a "hint" to the model.
	Description string `json:"description,omitempty"`
	// The MIME type of this resource, if known.
	MIMEType string `json:"mimeType,omitempty"`
	// Icons provides visual identifiers for the resource
	Icons []Icon `json:"icons,omitempty"`
	// Size is the size of the raw resource content, in bytes (i.e., before base64
	// encoding or any tokenization), if known. This can be used by hosts to
	// display file sizes and estimate context window usage.
	//
	// A pointer is used so that an explicit zero size remains distinguishable
	// from an unset value.
	Size *int64 `json:"size,omitempty"`
}

// GetName returns the name of the resource.
func (r Resource) GetName() string {
	return r.Name
}

// ResourceTemplate represents a template description for resources available
// on the server.
type ResourceTemplate struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta `json:"_meta,omitempty"`
	// A URI template (according to RFC 6570) that can be used to construct
	// resource URIs.
	URITemplate *URITemplate `json:"uriTemplate"`
	// A human-readable name for the type of resource this template refers to.
	//
	// This can be used by clients to populate UI elements.
	Name string `json:"name"`
	// Title is an optional human-readable, UI-friendly display name for this resource template.
	// If not provided, clients should fall back to Name.
	Title string `json:"title,omitempty"`
	// A description of what this template is for.
	//
	// This can be used by clients to improve the LLM's understanding of
	// available resources. It can be thought of like a "hint" to the model.
	Description string `json:"description,omitempty"`
	// The MIME type for all resources that match this template. This should only
	// be included if all resources matching this template have the same type.
	MIMEType string `json:"mimeType,omitempty"`
	// Icons provides visual identifiers for the resource template
	Icons []Icon `json:"icons,omitempty"`
}

// GetName returns the name of the resourceTemplate.
func (rt ResourceTemplate) GetName() string {
	return rt.Name
}

// ResourceContents represents the contents of a specific resource or sub-
// resource.
type ResourceContents interface {
	isResourceContents()
}

type TextResourceContents struct {
	// Raw per‑resource metadata; pass‑through as defined by MCP. Not the same as mcp.Meta.
	// Allows _meta to be used for MCP-UI features for example. Does not assume any specific format.
	Meta map[string]any `json:"_meta,omitempty"`
	// The URI of this resource.
	URI string `json:"uri"`
	// The MIME type of this resource, if known.
	MIMEType string `json:"mimeType,omitempty"`
	// The text of the item. This must only be set if the item can actually be
	// represented as text (not binary data).
	Text string `json:"text"`
}

func (TextResourceContents) isResourceContents() {}

type BlobResourceContents struct {
	// Raw per‑resource metadata; pass‑through as defined by MCP. Not the same as mcp.Meta.
	// Allows _meta to be used for MCP-UI features for example. Does not assume any specific format.
	Meta map[string]any `json:"_meta,omitempty"`
	// The URI of this resource.
	URI string `json:"uri"`
	// The MIME type of this resource, if known.
	MIMEType string `json:"mimeType,omitempty"`
	// A base64-encoded string representing the binary data of the item.
	Blob string `json:"blob"`
}

func (BlobResourceContents) isResourceContents() {}

package server

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/pbednarcik/flexmcp/mcp"
)

// resourceEntry holds both a resource and its handler
type resourceEntry struct {
	resource mcp.Resource
	handler  ResourceHandlerFunc
}

// resourceTemplateEntry holds a template and its handler, with the length of
// the template's literal prefix and its registration order, which pick the
// one served when several match a URI.
type resourceTemplateEntry struct {
	template mcp.ResourceTemplate
	handler  ResourceTemplateHandlerFunc
	literal  int
	order    uint64
}

// templateLiteralPrefix is the length of the text before the first
// expression of a URI template: the part a URI must carry as it is.
func templateLiteralPrefix(raw string) int {
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		return i
	}
	return len(raw)
}

// AddResources registers multiple resources at once
func (s *MCPServer) AddResources(resources ...ServerResource) {
	s.implicitlyRegisterResourceCapabilities()

	s.resourcesMu.Lock()
	s.addResourcesLocked(resources)
	s.resourcesMu.Unlock()

	s.notifyResourcesListChanged()
}

// addResourcesLocked is AddResources under resourcesMu.
func (s *MCPServer) addResourcesLocked(resources []ServerResource) {
	for _, entry := range resources {
		s.resources[entry.Resource.URI] = resourceEntry{
			resource: entry.Resource,
			handler:  entry.Handler,
		}
	}
}

// SetResources replaces all existing resources with the provided list, in
// one step: no read finds the resources gone between the old and the new.
func (s *MCPServer) SetResources(resources ...ServerResource) {
	s.implicitlyRegisterResourceCapabilities()

	s.resourcesMu.Lock()
	s.resources = make(map[string]resourceEntry, len(resources))
	s.addResourcesLocked(resources)
	s.resourcesMu.Unlock()

	s.notifyResourcesListChanged()
}

// AddResource registers a new resource and its handler
func (s *MCPServer) AddResource(
	resource mcp.Resource,
	handler ResourceHandlerFunc,
) {
	s.AddResources(ServerResource{Resource: resource, Handler: handler})
}

// DeleteResources removes resources from the server
func (s *MCPServer) DeleteResources(uris ...string) {
	s.resourcesMu.Lock()
	var exists bool
	for _, uri := range uris {
		if _, ok := s.resources[uri]; ok {
			delete(s.resources, uri)
			exists = true
		}
	}
	s.resourcesMu.Unlock()

	// Send notification to all initialized sessions if listChanged capability is enabled and we actually remove a resource
	if exists && s.capabilities.resources != nil && s.capabilities.resources.listChanged {
		s.SendNotificationToAllClients(mcp.MethodNotificationResourcesListChanged, nil)
	}
}

// ListResources returns a copy of the registered resources map.
func (s *MCPServer) ListResources() map[string]*ServerResource {
	s.resourcesMu.RLock()
	defer s.resourcesMu.RUnlock()
	if len(s.resources) == 0 {
		return nil
	}
	resourcesCopy := make(map[string]*ServerResource, len(s.resources))
	for uri, entry := range s.resources {
		resourcesCopy[uri] = &ServerResource{
			Resource: entry.resource,
			Handler:  entry.handler,
		}
	}
	return resourcesCopy
}

// RemoveResource removes a resource from the server
func (s *MCPServer) RemoveResource(uri string) {
	s.resourcesMu.Lock()
	_, exists := s.resources[uri]
	if exists {
		delete(s.resources, uri)
	}
	s.resourcesMu.Unlock()

	// Send notification to all initialized sessions if listChanged capability is enabled and we actually remove a resource
	if exists && s.capabilities.resources != nil && s.capabilities.resources.listChanged {
		s.SendNotificationToAllClients(mcp.MethodNotificationResourcesListChanged, nil)
	}
}

// AddResourceTemplates registers multiple resource templates at once
func (s *MCPServer) AddResourceTemplates(resourceTemplates ...ServerResourceTemplate) {
	s.implicitlyRegisterResourceCapabilities()

	s.resourcesMu.Lock()
	s.addResourceTemplatesLocked(resourceTemplates)
	s.resourcesMu.Unlock()

	s.notifyResourcesListChanged()
}

// addResourceTemplatesLocked is AddResourceTemplates under resourcesMu.
func (s *MCPServer) addResourceTemplatesLocked(resourceTemplates []ServerResourceTemplate) {
	for _, entry := range resourceTemplates {
		raw := entry.Template.URITemplate.Raw()
		// A template registered again keeps its place among the others.
		order := s.resourceTemplates[raw].order
		if order == 0 {
			s.resourceTemplateOrder++
			order = s.resourceTemplateOrder
		}
		s.resourceTemplates[raw] = resourceTemplateEntry{
			template: entry.Template,
			handler:  entry.Handler,
			literal:  templateLiteralPrefix(raw),
			order:    order,
		}
	}
}

// notifyResourcesListChanged tells every initialized session the resources
// changed, when the server declared the listChanged capability.
func (s *MCPServer) notifyResourcesListChanged() {
	if s.capabilities.resources.listChanged {
		s.SendNotificationToAllClients(mcp.MethodNotificationResourcesListChanged, nil)
	}
}

// SetResourceTemplates replaces all existing resource templates with the
// provided list, in one step: no read finds the templates gone between the
// old and the new.
func (s *MCPServer) SetResourceTemplates(templates ...ServerResourceTemplate) {
	s.implicitlyRegisterResourceCapabilities()

	s.resourcesMu.Lock()
	s.resourceTemplates = make(map[string]resourceTemplateEntry, len(templates))
	s.addResourceTemplatesLocked(templates)
	s.resourcesMu.Unlock()

	s.notifyResourcesListChanged()
}

// AddResourceTemplate registers a new resource template and its handler
func (s *MCPServer) AddResourceTemplate(
	template mcp.ResourceTemplate,
	handler ResourceTemplateHandlerFunc,
) {
	s.AddResourceTemplates(ServerResourceTemplate{Template: template, Handler: handler})
}

func (s *MCPServer) handleListResources(
	ctx context.Context,
	id any,
	request mcp.ListResourcesRequest,
) (*mcp.ListResourcesResult, *requestError) {
	s.resourcesMu.RLock()
	resourceMap := make(map[string]mcp.Resource, len(s.resources))
	for uri, entry := range s.resources {
		resourceMap[uri] = entry.resource
	}
	s.resourcesMu.RUnlock()

	// Sort the resources by name, then URI
	resourcesList := slices.SortedFunc(maps.Values(resourceMap), func(a, b mcp.Resource) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.URI, b.URI)
	})

	// Apply pagination
	resourcesToReturn, nextCursor, err := listByPagination(
		ctx,
		s,
		request.Params.Cursor,
		resourcesList,
	)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	result := mcp.ListResourcesResult{
		Resources:  resourcesToReturn,
		NextCursor: nextCursor,
	}
	return &result, nil
}

func (s *MCPServer) handleListResourceTemplates(
	ctx context.Context,
	id any,
	request mcp.ListResourceTemplatesRequest,
) (*mcp.ListResourceTemplatesResult, *requestError) {
	// Get global templates
	s.resourcesMu.RLock()
	templateMap := make(map[string]mcp.ResourceTemplate, len(s.resourceTemplates))
	for uri, entry := range s.resourceTemplates {
		templateMap[uri] = entry.template
	}
	s.resourcesMu.RUnlock()

	// Convert map to slice for sorting and pagination
	templates := make([]mcp.ResourceTemplate, 0, len(templateMap))
	for _, template := range templateMap {
		templates = append(templates, template)
	}

	// Sort the templates by name, then URI template
	slices.SortFunc(templates, func(a, b mcp.ResourceTemplate) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(pageTieBreak(&a), pageTieBreak(&b))
	})
	templatesToReturn, nextCursor, err := listByPagination(
		ctx,
		s,
		request.Params.Cursor,
		templates,
	)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}
	result := mcp.ListResourceTemplatesResult{
		ResourceTemplates: templatesToReturn,
		NextCursor:        nextCursor,
	}
	return &result, nil
}

func (s *MCPServer) handleReadResource(
	ctx context.Context,
	id any,
	request mcp.ReadResourceRequest,
) (*mcp.ReadResourceResult, *requestError) {
	s.resourcesMu.RLock()

	// First try direct resource handlers
	if globalResource, ok := s.resources[request.Params.URI]; ok {
		handler := globalResource.handler
		s.resourcesMu.RUnlock()

		contents, err := s.wrapResourceHandler(handler)(ctx, request)
		if err != nil {
			return nil, handlerError(ctx, id, err)
		}
		return readResourceResult(contents), nil
	}

	// No direct handler: the templates are tried. Among those matching the
	// URI, the one with the longest literal prefix is served, and between
	// equal prefixes the one registered first, so two templates matching a
	// URI answer the same way every time.
	var best *resourceTemplateEntry
	for _, entry := range s.resourceTemplates {
		if entry.template.URITemplate == nil || !matchesTemplate(request.Params.URI, entry.template.URITemplate) {
			continue
		}
		if best == nil || entry.literal > best.literal || (entry.literal == best.literal && entry.order < best.order) {
			best = &entry
		}
	}
	var matchedHandler ResourceTemplateHandlerFunc
	matched := best != nil
	if matched {
		matchedHandler = best.handler
		matchedVars := best.template.URITemplate.Match(request.Params.URI)
		// Convert matched variables to a map
		request.Params.Arguments = make(map[string]any, len(matchedVars))
		for name, value := range matchedVars {
			request.Params.Arguments[name] = value.V
		}
	}
	s.resourcesMu.RUnlock()

	if matched {
		contents, err := s.wrapResourceHandler(ResourceHandlerFunc(matchedHandler))(ctx, request)
		if err != nil {
			return nil, handlerError(ctx, id, err)
		}
		return readResourceResult(contents), nil
	}

	// The URI rides in the error's data, as the specification's examples
	// have it in both eras.
	return nil, &requestError{
		id:   id,
		code: resourceNotFoundCode(ctx),
		err: fmt.Errorf(
			"handler not found for resource URI '%s': %w",
			request.Params.URI,
			ErrResourceNotFound,
		),
		data: map[string]any{"uri": request.Params.URI},
	}
}

// wrapResourceHandler returns handler inside the resource middlewares,
// outermost first, the chain built outside the lock (see
// toolFilterSnapshot).
func (s *MCPServer) wrapResourceHandler(handler ResourceHandlerFunc) ResourceHandlerFunc {
	s.resourceMiddlewareMu.RLock()
	mw := s.resourceHandlerMiddlewares
	s.resourceMiddlewareMu.RUnlock()
	for _, m := range slices.Backward(mw) {
		handler = m(handler)
	}
	return handler
}

// readResourceResult wraps the contents a resource handler returned. contents
// is a required array, so a handler that found nothing still sends [] rather
// than null.
func readResourceResult(contents []mcp.ResourceContents) *mcp.ReadResourceResult {
	if contents == nil {
		contents = []mcp.ResourceContents{}
	}
	return &mcp.ReadResourceResult{Contents: contents}
}

// matchesTemplate checks if a URI matches a URI template pattern
func matchesTemplate(uri string, template *mcp.URITemplate) bool {
	return template.Regexp().MatchString(uri)
}

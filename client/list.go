package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pbednarcik/flexmcp/mcp"
)

// ListResourcesByPage manually list resources by page.
func (c *Client) ListResourcesByPage(
	ctx context.Context,
	request mcp.ListResourcesRequest,
) (*mcp.ListResourcesResult, error) {
	result, err := listByPage[mcp.ListResourcesResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodResourcesList))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ErrRepeatedListCursor is returned when a paginated list repeats a cursor
// that was already requested. Following it would request the same page again.
var ErrRepeatedListCursor = errors.New("list cursor did not advance")

// noteListCursor records next before it is requested. A cursor already in
// seen means the server did not advance.
func noteListCursor(seen map[mcp.Cursor]struct{}, next mcp.Cursor) error {
	if _, dup := seen[next]; dup {
		return fmt.Errorf("%w: %q", ErrRepeatedListCursor, next)
	}
	seen[next] = struct{}{}
	return nil
}

// seenCursors starts the set of cursors a walk has requested: the caller's
// own, when it gave one.
func seenCursors(start *mcp.Cursor) map[mcp.Cursor]struct{} {
	seen := map[mcp.Cursor]struct{}{}
	if start != nil {
		seen[*start] = struct{}{}
	}
	return seen
}

// ListResources lists all resources by following paginated responses.
func (c *Client) ListResources(
	ctx context.Context,
	request mcp.ListResourcesRequest,
) (*mcp.ListResourcesResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListResourcesByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListResourcesByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Resources = append(result.Resources, newPageRes.Resources...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	return result, nil
}

// ListResourceTemplatesByPage manually lists resource templates by page.
func (c *Client) ListResourceTemplatesByPage(
	ctx context.Context,
	request mcp.ListResourceTemplatesRequest,
) (*mcp.ListResourceTemplatesResult, error) {
	result, err := listByPage[mcp.ListResourceTemplatesResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodResourcesTemplatesList))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListResourceTemplates lists all resource templates by following paginated responses.
func (c *Client) ListResourceTemplates(
	ctx context.Context,
	request mcp.ListResourceTemplatesRequest,
) (*mcp.ListResourceTemplatesResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListResourceTemplatesByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListResourceTemplatesByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.ResourceTemplates = append(result.ResourceTemplates, newPageRes.ResourceTemplates...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	return result, nil
}

// ListPromptsByPage manually lists prompts by page.
func (c *Client) ListPromptsByPage(
	ctx context.Context,
	request mcp.ListPromptsRequest,
) (*mcp.ListPromptsResult, error) {
	result, err := listByPage[mcp.ListPromptsResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodPromptsList))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListPrompts lists all prompts by following paginated responses.
func (c *Client) ListPrompts(
	ctx context.Context,
	request mcp.ListPromptsRequest,
) (*mcp.ListPromptsResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListPromptsByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListPromptsByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Prompts = append(result.Prompts, newPageRes.Prompts...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	return result, nil
}

// ListToolsByPage manually lists tools by page.
func (c *Client) ListToolsByPage(
	ctx context.Context,
	request mcp.ListToolsRequest,
) (*mcp.ListToolsResult, error) {
	result, err := listByPage[mcp.ListToolsResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodToolsList))
	if err != nil {
		return nil, err
	}
	result.Tools = c.withoutInvalidHeaderTools(ctx, result.Tools)
	// Cache the definitions so that tools/call requests can mirror
	// x-mcp-header annotated parameters into HTTP headers (SEP-2243).
	c.rememberTools(result.Tools)
	return result, nil
}

// ListTools lists all tools by following paginated responses.
func (c *Client) ListTools(
	ctx context.Context,
	request mcp.ListToolsRequest,
) (*mcp.ListToolsResult, error) {
	seen := seenCursors(request.Params.Cursor)
	whole := request.Params.Cursor == nil
	result, err := c.ListToolsByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListToolsByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Tools = append(result.Tools, newPageRes.Tools...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	// A walk from the first page is the server's whole list: the cache
	// becomes it, and a tool the server stopped listing is forgotten. A walk
	// from a caller's cursor is part of the list; its pages merged in.
	if whole {
		c.replaceTools(result.Tools)
	}
	return result, nil
}

func listByPage[T any](
	ctx context.Context,
	client *Client,
	request mcp.PaginatedRequest,
	header http.Header,
	method string,
) (*T, error) {
	response, err := client.sendRequest(ctx, method, request.Params, outboundHeader(header, request.Method))
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	if metadata, ok := any(&result).(mcp.ResultMetadata); ok {
		if err := client.checkResultType(metadata.GetResultType()); err != nil {
			return nil, err
		}
	}
	return &result, nil
}

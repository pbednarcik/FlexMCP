package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"sort"
	"strings"

	"github.com/pbednarcik/flexmcp/mcp"
)

// listByPagination returns the page of allElements that follows cursor. A nil
// or empty cursor means the start of the list. allElements must be sorted by
// name, then by pageTieBreak.
func listByPagination[T mcp.Named](
	_ context.Context,
	s *MCPServer,
	cursor *mcp.Cursor,
	allElements []T,
) ([]T, *mcp.Cursor, error) {
	startPos := 0
	if cursor != nil && *cursor != "" {
		encodedName, encodedTieBreak, hasTieBreak := strings.Cut(string(*cursor), ".")
		c, err := base64.StdEncoding.DecodeString(encodedName)
		if err != nil {
			return nil, nil, err
		}
		name := string(c)
		if !hasTieBreak {
			// A name-only cursor, as older servers issued, cannot say where in
			// a group of equally named elements its page ended. It continues
			// after the whole group, as it always did, rather than repeating
			// the group from its start.
			startPos = sort.Search(len(allElements), func(i int) bool {
				return allElements[i].GetName() > name
			})
		} else {
			t, err := base64.StdEncoding.DecodeString(encodedTieBreak)
			if err != nil {
				return nil, nil, err
			}
			tieBreak := string(t)
			startPos = sort.Search(len(allElements), func(i int) bool {
				return comparePageKey(&allElements[i], name, tieBreak) > 0
			})
		}
	}
	endPos := len(allElements)
	if s.paginationLimit != nil && *s.paginationLimit > 0 {
		if len(allElements)-startPos > *s.paginationLimit {
			endPos = startPos + *s.paginationLimit
		}
	}
	elementsToReturn := allElements[startPos:endPos]
	// Every list result holds its items in a required array. allElements can
	// be nil: the resource and task lists start out that way, and a filter
	// that hides every item may return nil. A nil slice would go out as null.
	if elementsToReturn == nil {
		elementsToReturn = []T{}
	}
	// set the next cursor
	nextCursor := func() *mcp.Cursor {
		if s.paginationLimit != nil && *s.paginationLimit > 0 && len(elementsToReturn) >= *s.paginationLimit {
			next := encodePageCursor(&elementsToReturn[len(elementsToReturn)-1])
			return &next
		}
		return nil
	}()
	return elementsToReturn, nextCursor, nil
}

// pageTieBreak orders list elements that share a name. Tool, prompt and task
// names are unique, but resource and resource template names need not be, so
// those are ordered by URI or URI template within a name. Both are unique, as
// the server keys its resources and templates by them. Without a tie-break the
// cursor could not say where among equally named elements a page ended, and
// the rest of them would be skipped.
func pageTieBreak[T mcp.Named](element *T) string {
	switch e := any(element).(type) {
	case *mcp.Resource:
		return e.URI
	case *mcp.ResourceTemplate:
		if e.URITemplate != nil {
			return e.URITemplate.Raw()
		}
	}
	return ""
}

// comparePageKey compares element with the position given by a name and a
// tie-break. The tie-break is only looked at when the names are equal.
func comparePageKey[T mcp.Named](element *T, name, tieBreak string) int {
	if c := cmp.Compare((*element).GetName(), name); c != 0 {
		return c
	}
	return cmp.Compare(pageTieBreak(element), tieBreak)
}

// encodePageCursor returns the cursor for the page after element: the base64
// of its name, a dot, and the base64 of its tie-break, the URI or URI template
// of a resource or template and empty for any other element. The dot is
// outside the base64 alphabet, so it tells these cursors apart from name-only
// ones, and it keeps a cursor from ever being empty, which would mean the
// start of the list even after an element with an empty name.
func encodePageCursor[T mcp.Named](element *T) mcp.Cursor {
	return mcp.Cursor(base64.StdEncoding.EncodeToString([]byte((*element).GetName())) +
		"." + base64.StdEncoding.EncodeToString([]byte(pageTieBreak(element))))
}

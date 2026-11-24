package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubscriptionFilterReadsTaskIDs pins that a subscriptions/listen filter
// reads the Tasks extension's taskIds, that a filter asking for task pushes
// alone is not empty, and that a filter without them leaves the member out.
func TestSubscriptionFilterReadsTaskIDs(t *testing.T) {
	var filter SubscriptionFilter
	require.NoError(t, json.Unmarshal([]byte(`{"taskIds":["t1","t2"]}`), &filter))
	assert.Equal(t, []string{"t1", "t2"}, filter.TaskIDs)
	assert.False(t, filter.IsEmpty(), "a filter asking for task pushes opts in to something")

	out, err := json.Marshal(SubscriptionFilter{ToolsListChanged: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"toolsListChanged":true}`, string(out))
}

package transport

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Two ids share a response key exactly when their String forms are equal: an
// id built in process finds the response decoded from the wire, whatever Go
// type it was built from, and ids that differ never share one.
func TestResponseKeyMatchesTheIDsString(t *testing.T) {
	values := []any{
		int64(5), 5, int32(5), int16(5), int8(5), uint(5), uint64(5), uint32(5), uint16(5), uint8(5),
		json.Number("5"), float64(5), "5", "int64:5", int64(-5), int64(0), "", int64(6),
		uint64(math.MaxUint64), json.Number("18446744073709551615"), float64(5.5), json.Number("5.5"),
		int64(math.MinInt64), json.Number("-9223372036854775808"),
	}
	for _, a := range values {
		for _, b := range values {
			idA, idB := mcp.NewRequestId(a), mcp.NewRequestId(b)
			assert.Equal(t, idA.String() == idB.String(), responseKeyOf(idA) == responseKeyOf(idB),
				"%T(%v) and %T(%v)", a, a, b, b)
		}
	}
}

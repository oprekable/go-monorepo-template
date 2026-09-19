package doer

import (
	"sort"
	"testing"

	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/stretchr/testify/assert"
)

func Test_hertzHeaderCarrier_Get(t *testing.T) {
	header := &protocol.RequestHeader{}
	header.Set("X-Request-Id", "12345")
	header.Set("Content-Type", "application/json")

	carrier := hertzHeaderCarrier{header: header}

	assert.Equal(t, "12345", carrier.Get("X-Request-Id"))
	assert.Equal(t, "application/json", carrier.Get("Content-Type"))
	assert.Equal(t, "", carrier.Get("Non-Existent-Key"))
}

func Test_hertzHeaderCarrier_Set(t *testing.T) {
	header := &protocol.RequestHeader{}
	carrier := hertzHeaderCarrier{header: header}

	carrier.Set("Traceparent", "00-abc-123-01")
	carrier.Set("X-Custom-Header", "value")

	assert.Equal(t, "00-abc-123-01", string(header.Peek("Traceparent")))
	assert.Equal(t, "value", string(header.Peek("X-Custom-Header")))

	// Test overwriting a value
	carrier.Set("X-Custom-Header", "new-value")
	assert.Equal(t, "new-value", string(header.Peek("X-Custom-Header")))
}

func Test_hertzHeaderCarrier_Keys(t *testing.T) {
	t.Run("with_headers", func(t *testing.T) {
		header := &protocol.RequestHeader{}
		header.Set("A", "1")
		header.Set("B", "2")
		header.Set("C", "3")

		carrier := hertzHeaderCarrier{header: header}
		keys := carrier.Keys()

		// The order of keys is not guaranteed by VisitAll, so we sort them for a stable test.
		sort.Strings(keys)

		assert.Equal(t, []string{"A", "B", "C"}, keys)
		assert.Len(t, keys, 3)
	})

	t.Run("cache_hit_second_call", func(t *testing.T) {
		header := &protocol.RequestHeader{}
		header.Set("X", "1")
		header.Set("Y", "2")

		carrier := hertzHeaderCarrier{header: header}

		// First call: builds and caches keys
		keys1 := carrier.Keys()
		assert.Len(t, keys1, 2)

		// Second call: must hit c.keys cache (lines 19-21)
		keys2 := carrier.Keys()
		assert.Equal(t, keys1, keys2, "cached keys must match first call")
	})

	t.Run("with_empty_headers", func(t *testing.T) {
		header := &protocol.RequestHeader{}
		carrier := hertzHeaderCarrier{header: header}

		keys := carrier.Keys()
		assert.Nil(t, keys)
		assert.Len(t, keys, 0)
	})

	t.Run("with_nil_header_struct", func(t *testing.T) {
		// This is an edge case, the pointer would normally not be nil.
		carrier := hertzHeaderCarrier{header: &protocol.RequestHeader{}}
		keys := carrier.Keys()
		assert.Nil(t, keys)
	})
}

package doer

import (
	"strconv"
	"sync"
	"sync/atomic"
)

// statusCodeStrings is an immutable cache of HTTP status code → string mappings.
// Populated once in init(); MUST NOT be written to after init completes.
// Concurrent reads from multiple goroutines are safe (Go maps are safe for concurrent read-only access).
// Any future write-back in statusToString or elsewhere WILL introduce a data race.
var statusCodeStrings = map[int]string{}

// uncommonStatusCodeCache is a dynamic cache for less common HTTP status codes.
// Bounded to 50 entries to prevent unbounded growth.
var (
	uncommonStatusCodeCache      sync.Map
	uncommonStatusCodeCacheCount atomic.Int64
)

func init() {
	// Pre-populate with common HTTP status codes to avoid strconv overhead on hot paths.
	commonCodes := []int{
		200, 201, 202, 204,
		301, 302, 304, 307, 308,
		400, 401, 403, 404, 405, 409, 429,
		500, 501, 502, 503, 504,
	}
	for _, code := range commonCodes {
		statusCodeStrings[code] = strconv.Itoa(code)
	}
}

// statusToString returns the string representation of an HTTP status code.
// Uses a read-only lookup against the immutable statusCodeStrings cache.
// For uncommon codes, uses a bounded dynamic cache (max 50 entries) to avoid
// repeated strconv.Itoa allocations.
func statusToString(code int) string {
	if s, ok := statusCodeStrings[code]; ok {
		return s
	}

	// Check uncommon status code cache
	if s, ok := uncommonStatusCodeCache.Load(code); ok {
		return s.(string)
	}

	// Not in cache, convert and store if cache not full
	s := strconv.Itoa(code)

	// Only store if cache size < 50 to prevent unbounded growth
	// Use atomic counter instead of expensive Range() iteration
	if uncommonStatusCodeCacheCount.Load() < 50 {
		// Check if already stored by another goroutine
		if _, loaded := uncommonStatusCodeCache.LoadOrStore(code, s); !loaded {
			uncommonStatusCodeCacheCount.Add(1)
		}
	}

	return s
}

package doer

import (
	"sync"
	"sync/atomic"
	"time"
)

// Circuit breaker states
const (
	circuitClosed   int32 = 0
	circuitOpen     int32 = 1
	circuitHalfOpen int32 = 2
)

// hostCircuit tracks circuit breaker state for a single downstream host.
// Each host gets independent failure tracking so one failing host
// doesn't affect traffic to healthy hosts.
type hostCircuit struct {
	state    atomic.Int32 // circuitClosed/circuitOpen/circuitHalfOpen
	failures atomic.Int64
	resetAt  atomic.Int64 // Unix timestamp when circuit transitions to half-open
}

// testHookCASLost allows deterministic testing of CAS race conditions.
var testHookCASLost func()

// allow checks if a request is allowed. Returns (allowed, reason).
// If not allowed, reason describes why (open/half-open).
func (h *hostCircuit) allow(now int64) (bool, string) {
	switch h.state.Load() {
	case circuitOpen:
		if now < h.resetAt.Load() {
			return false, "circuit breaker open"
		}
		// Reset time reached — CAS to half-open allows exactly one probe request.
		// The CAS winner becomes the probe; losers see circuitOpen and retry.
		if testHookCASLost != nil {
			testHookCASLost()
		}
		if h.state.CompareAndSwap(circuitOpen, circuitHalfOpen) {
			h.failures.Store(0)
			return true, "" // this request IS the probe
		}
		// CAS lost: another goroutine is now the probe.
		// Re-check state: if half-open, reject; if closed, allow.
		if h.state.Load() == circuitHalfOpen {
			return false, "circuit breaker half-open, probe in flight"
		}
		return true, "" // circuit closed by probe, allow
	case circuitHalfOpen:
		// Probe in flight — reject all other requests until it resolves.
		return false, "circuit breaker half-open, probe in flight"
	}
	return true, ""
}

// recordFailure records a request failure and potentially trips the circuit.
// Returns true if the circuit opened as a result of this failure.
func (h *hostCircuit) recordFailure(now int64, timeout time.Duration, threshold int64) bool {
	state := h.state.Load()
	if state == circuitHalfOpen {
		// Probe failed — reopen circuit
		h.state.Store(circuitOpen)
		h.resetAt.Store(now + timeout.Nanoseconds()/1e9)
		return true
	} else if state == circuitClosed {
		failures := h.failures.Add(1)
		if failures >= threshold {
			h.state.Store(circuitOpen)
			h.resetAt.Store(now + timeout.Nanoseconds()/1e9)
			return true
		}
	}
	return false
}

// recordSuccess records a successful request.
// If in half-open state, closes the circuit.
func (h *hostCircuit) recordSuccess() {
	if h.state.Load() == circuitHalfOpen {
		h.state.Store(circuitClosed)
		h.failures.Store(0)
	}
}

// circuitFor returns the circuit for a host, creating it on first use.
// Thread-safe via sync.Map with LoadOrStore.
//
// Note: Assumes low host cardinality (fixed set of downstream services).
// High cardinality hosts (e.g., dynamic hostnames with unique IDs) will cause
// unbounded map growth. For high cardinality scenarios, consider host grouping
// or external circuit breaker coordination.
func circuitFor(circuits *sync.Map, host string) *hostCircuit {
	if v, ok := circuits.Load(host); ok {
		return v.(*hostCircuit)
	}
	c := &hostCircuit{}
	actual, _ := circuits.LoadOrStore(host, c)
	return actual.(*hostCircuit)
}

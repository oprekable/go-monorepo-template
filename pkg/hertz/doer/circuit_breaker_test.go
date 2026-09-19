package doer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHostCircuit_allow_ClosedState(t *testing.T) {
	c := &hostCircuit{}
	allowed, reason := c.allow(100)
	assert.True(t, allowed)
	assert.Empty(t, reason)
}

func TestHostCircuit_allow_OpenState(t *testing.T) {
	c := &hostCircuit{}
	c.state.Store(circuitOpen)
	c.resetAt.Store(200)

	t.Run("before_reset_rejects", func(t *testing.T) {
		allowed, reason := c.allow(100)
		assert.False(t, allowed)
		assert.Equal(t, "circuit breaker open", reason)
	})

	t.Run("at_reset_becomes_half_open_probe", func(t *testing.T) {
		c2 := &hostCircuit{}
		c2.state.Store(circuitOpen)
		c2.resetAt.Store(200)
		c2.failures.Store(5)

		allowed, reason := c2.allow(200)
		assert.True(t, allowed, "probe request should be allowed")
		assert.Empty(t, reason)
		assert.Equal(t, circuitHalfOpen, c2.state.Load())
		assert.Equal(t, int64(0), c2.failures.Load(), "failures reset on probe")
	})

	t.Run("after_reset_becomes_half_open_probe", func(t *testing.T) {
		c3 := &hostCircuit{}
		c3.state.Store(circuitOpen)
		c3.resetAt.Store(200)

		allowed, reason := c3.allow(300)
		assert.True(t, allowed)
		assert.Empty(t, reason)
		assert.Equal(t, circuitHalfOpen, c3.state.Load())
	})
}

func TestHostCircuit_allow_HalfOpenState(t *testing.T) {
	c := &hostCircuit{}
	c.state.Store(circuitHalfOpen)

	allowed, reason := c.allow(100)
	assert.False(t, allowed)
	assert.Equal(t, "circuit breaker half-open, probe in flight", reason)
}

func TestHostCircuit_allow_CASLostToHalfOpen(t *testing.T) {
	// Simulate CAS loss: another goroutine won the probe race.
	c := &hostCircuit{}
	c.state.Store(circuitOpen)
	c.resetAt.Store(50)

	// Setup hook to change state before CAS
	defer func() { testHookCASLost = nil }()
	testHookCASLost = func() {
		c.state.Store(circuitHalfOpen) // simulate another goroutine winning
	}

	allowed, reason := c.allow(100)
	assert.False(t, allowed)
	assert.Contains(t, reason, "half-open")
}

func TestHostCircuit_allow_CASLostToClosed(t *testing.T) {
	// Simulate CAS loss: another goroutine won the probe race and quickly reported success (state -> closed).
	c := &hostCircuit{}
	c.state.Store(circuitOpen)
	c.resetAt.Store(50)

	// Setup hook to change state before CAS
	defer func() { testHookCASLost = nil }()
	testHookCASLost = func() {
		c.state.Store(circuitClosed) // simulate another goroutine winning and succeeding
	}

	allowed, reason := c.allow(100)
	assert.True(t, allowed)
	assert.Empty(t, reason)
}

func TestHostCircuit_recordFailure_ClosedToOpen(t *testing.T) {
	c := &hostCircuit{}
	timeout := 10 * time.Second
	threshold := int64(3)

	t.Run("below_threshold_stays_closed", func(t *testing.T) {
		opened := c.recordFailure(100, timeout, threshold)
		assert.False(t, opened)
		assert.Equal(t, circuitClosed, c.state.Load())
		assert.Equal(t, int64(1), c.failures.Load())
	})

	t.Run("at_threshold_trips_open", func(t *testing.T) {
		c.recordFailure(100, timeout, threshold)           // failures=2
		opened := c.recordFailure(100, timeout, threshold) // failures=3
		assert.True(t, opened)
		assert.Equal(t, circuitOpen, c.state.Load())
		assert.Equal(t, int64(110), c.resetAt.Load()) // 100 + 10s
	})
}

func TestHostCircuit_recordFailure_HalfOpenProbeFails(t *testing.T) {
	c := &hostCircuit{}
	c.state.Store(circuitHalfOpen)
	timeout := 5 * time.Second

	opened := c.recordFailure(200, timeout, 3)
	assert.True(t, opened)
	assert.Equal(t, circuitOpen, c.state.Load())
	assert.Equal(t, int64(205), c.resetAt.Load())
}

func TestHostCircuit_recordFailure_OpenStateIgnored(t *testing.T) {
	c := &hostCircuit{}
	c.state.Store(circuitOpen)
	c.resetAt.Store(999)

	opened := c.recordFailure(100, 5*time.Second, 3)
	assert.False(t, opened, "failure in open state should not change anything")
	assert.Equal(t, circuitOpen, c.state.Load())
	assert.Equal(t, int64(999), c.resetAt.Load(), "resetAt unchanged")
}

func TestHostCircuit_recordSuccess_HalfOpenCloses(t *testing.T) {
	c := &hostCircuit{}
	c.state.Store(circuitHalfOpen)
	c.failures.Store(10)

	c.recordSuccess()
	assert.Equal(t, circuitClosed, c.state.Load())
	assert.Equal(t, int64(0), c.failures.Load())
}

func TestHostCircuit_recordSuccess_ClosedNoOp(t *testing.T) {
	c := &hostCircuit{}
	c.failures.Store(2)

	c.recordSuccess()
	assert.Equal(t, circuitClosed, c.state.Load())
	assert.Equal(t, int64(2), c.failures.Load(), "failures unchanged in closed state")
}

func TestHostCircuit_concurrentProbe(t *testing.T) {
	// Many goroutines race to enter half-open. Exactly one wins the probe.
	c := &hostCircuit{}
	c.state.Store(circuitOpen)
	c.resetAt.Store(0) // already past due

	const goroutines = 100
	var allowedCount atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			allowed, _ := c.allow(100)
			if allowed {
				allowedCount.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), allowedCount.Load(), "exactly one probe should win")
	assert.Equal(t, circuitHalfOpen, c.state.Load())
}

func TestCircuitFor_NewHost(t *testing.T) {
	var circuits sync.Map
	c := circuitFor(&circuits, "host-a")
	assert.NotNil(t, c)
	assert.Equal(t, circuitClosed, c.state.Load())
}

func TestCircuitFor_SameHostReturnsSameInstance(t *testing.T) {
	var circuits sync.Map
	c1 := circuitFor(&circuits, "host-a")
	c2 := circuitFor(&circuits, "host-a")
	assert.Same(t, c1, c2)
}

func TestCircuitFor_DifferentHostsDifferentInstances(t *testing.T) {
	var circuits sync.Map
	c1 := circuitFor(&circuits, "host-a")
	c2 := circuitFor(&circuits, "host-b")
	assert.NotSame(t, c1, c2)
}

func TestCircuitFor_ConcurrentSameHost(t *testing.T) {
	var circuits sync.Map
	const goroutines = 50
	results := make([]*hostCircuit, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			results[idx] = circuitFor(&circuits, "host-x")
		}(i)
	}
	wg.Wait()

	for i := 1; i < goroutines; i++ {
		assert.Same(t, results[0], results[i], "all goroutines should get same circuit")
	}
}

func TestHostCircuit_fullCycle(t *testing.T) {
	// End-to-end: closed → failures → open → timeout → half-open → success → closed
	c := &hostCircuit{}
	timeout := 10 * time.Second
	threshold := int64(3)
	now := int64(100)

	// Accumulate failures
	for range threshold {
		c.recordFailure(now, timeout, threshold)
	}
	assert.Equal(t, circuitOpen, c.state.Load())

	// Before timeout: rejected
	allowed, _ := c.allow(now + 5)
	assert.False(t, allowed)

	// After timeout: probe allowed
	allowed, _ = c.allow(now + 11)
	assert.True(t, allowed)
	assert.Equal(t, circuitHalfOpen, c.state.Load())

	// Probe succeeds → closed
	c.recordSuccess()
	assert.Equal(t, circuitClosed, c.state.Load())
	assert.Equal(t, int64(0), c.failures.Load())
}

func TestHostCircuit_halfOpenFailureCycle(t *testing.T) {
	// closed → open → half-open → probe fails → open again
	c := &hostCircuit{}
	timeout := 10 * time.Second
	threshold := int64(2)
	now := int64(100)

	// Trip circuit
	c.recordFailure(now, timeout, threshold)
	c.recordFailure(now, timeout, threshold)
	assert.Equal(t, circuitOpen, c.state.Load())

	// Probe after timeout
	allowed, _ := c.allow(now + 11)
	assert.True(t, allowed)
	assert.Equal(t, circuitHalfOpen, c.state.Load())

	// Probe fails → reopen
	opened := c.recordFailure(now+11, timeout, threshold)
	assert.True(t, opened)
	assert.Equal(t, circuitOpen, c.state.Load())
	assert.Equal(t, now+21, c.resetAt.Load()) // (now+11) + 10s
}

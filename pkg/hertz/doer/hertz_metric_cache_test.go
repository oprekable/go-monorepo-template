package doer

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

// store adds or updates an entry in the cache (for testing).
func (mc *MetricCache) Store(key string, entry *metricCacheEntry) {
	mc.cache.Store(key, entry)
}

// Range iterates over all cache entries (for testing).
func (mc *MetricCache) Range(f func(key, value any) bool) {
	mc.cache.Range(f)
}

// Size returns the current cache size (for testing).
func (mc *MetricCache) Size() int64 {
	return mc.size.Load()
}

// SetSize sets the cache size (for testing).
func (mc *MetricCache) SetSize(size int64) {
	mc.size.Store(size)
}

// LastEviction returns the last eviction timestamp (for testing).
func (mc *MetricCache) LastEviction() int64 {
	return mc.lastEviction.Load()
}

// SetLastEviction sets the last eviction timestamp (for testing).
func (mc *MetricCache) SetLastEviction(ts int64) {
	mc.lastEviction.Store(ts)
}

// Delete removes an entry from the cache (for testing).
func (mc *MetricCache) Delete(key string) {
	mc.cache.Delete(key)
}

func TestNewMetricCache_NilInputs(t *testing.T) {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_counter"}, []string{"method", "route", "status"})
	histogram := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_histogram"}, []string{"method", "route"})

	assert.Nil(t, NewMetricCache(nil, histogram, nil, nil))
	assert.Nil(t, NewMetricCache(counter, nil, nil, nil))
	assert.Nil(t, NewMetricCache(nil, nil, nil, nil))
	assert.NotNil(t, NewMetricCache(counter, histogram, nil, nil))
}

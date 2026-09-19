package doer

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metricCacheEntry wraps MetricCollectors with a timestamp for TTL-based eviction.
// Entries not accessed within metricCacheTTL (default 1 hour) are evicted to prevent
// unbounded cache growth from route cardinality explosion.
type metricCacheEntry struct {
	mc       MetricCollectors
	lastUsed atomic.Int64 // Unix timestamp, updated on cache hit
}

// MetricCache provides cached metric collectors to avoid repeated WithLabelValues calls.
// Thread-safe for concurrent access.
type MetricCache struct {
	builderPool    sync.Pool
	cacheHits      prometheus.Counter
	cacheMisses    prometheus.Counter
	counter        *prometheus.CounterVec
	histogram      *prometheus.HistogramVec
	cache          sync.Map
	size           atomic.Int64
	lastEviction   atomic.Int64
	maxEntries     int64
	evictionTTL    int64
	evictionPeriod int64
}

// NewMetricCache creates a metric cache with the given prometheus metrics.
// Returns nil if counter or histogram is nil (metrics disabled).
func NewMetricCache(
	counter *prometheus.CounterVec,
	histogram *prometheus.HistogramVec,
	cacheHits prometheus.Counter,
	cacheMisses prometheus.Counter,
) *MetricCache {
	if counter == nil || histogram == nil {
		return nil
	}

	mc := &MetricCache{
		counter:        counter,
		histogram:      histogram,
		cacheHits:      cacheHits,
		cacheMisses:    cacheMisses,
		maxEntries:     MaxMetricCacheEntries,
		evictionTTL:    metricCacheTTL,
		evictionPeriod: 60,
	}

	mc.builderPool = sync.Pool{
		New: func() any {
			return new(strings.Builder)
		},
	}

	return mc
}

// Get returns cached metric collectors for the given method, route, and status.
// Handles cache misses, overflow, and rate-limited eviction internally.
func (mc *MetricCache) Get(method, route string, status int, now time.Time) (m MetricCollectors) {
	statusStr := statusToString(status)

	keyBuilder := mc.builderPool.Get().(*strings.Builder)
	keyBuilder.Reset()
	keyBuilder.Grow(len(method) + len(route) + MetricKeyOverhead)
	keyBuilder.WriteString(method)
	keyBuilder.WriteByte(':')
	keyBuilder.WriteString(route)
	keyBuilder.WriteByte(':')
	keyBuilder.WriteString(statusStr)

	key := keyBuilder.String()
	if keyBuilder.Cap() <= maxPooledBuilderCap {
		mc.builderPool.Put(keyBuilder)
	}

	nowUnix := now.Unix()

	// Try load from cache first — works even when cache is full (lock-free read).
	if val, ok := mc.cache.Load(key); ok {
		entry := val.(*metricCacheEntry)
		entry.lastUsed.Store(nowUnix)
		if mc.cacheHits != nil {
			mc.cacheHits.Inc()
		}
		m = entry.mc
		return
	}

	// Check if cache is full BEFORE incrementing miss counter.
	if mc.size.Load() >= mc.maxEntries {
		// Rate-limit eviction: only attempt every 60 seconds
		if nowUnix-mc.lastEviction.Load() >= mc.evictionPeriod {
			mc.lastEviction.Store(nowUnix)

			evicted := 0
			mc.cache.Range(func(k, v any) bool {
				entry := v.(*metricCacheEntry)
				if nowUnix-entry.lastUsed.Load() > mc.evictionTTL {
					mc.cache.Delete(k)
					mc.size.Add(-1)
					evicted++
				}
				return true
			})

			if evicted == 0 || mc.size.Load() >= mc.maxEntries {
				m = MetricCollectors{
					counter:   mc.counter.WithLabelValues(method, "/_overflow", statusStr),
					histogram: mc.histogram.WithLabelValues(method, "/_overflow"),
				}

				return
			}
		} else {
			m = MetricCollectors{
				counter:   mc.counter.WithLabelValues(method, "/_overflow", statusStr),
				histogram: mc.histogram.WithLabelValues(method, "/_overflow"),
			}

			return
		}
	}

	// Cache miss with available slots
	if mc.cacheMisses != nil {
		mc.cacheMisses.Inc()
	}

	// Optimistic slot reservation via CAS
	for {
		cur := mc.size.Load()
		if cur >= mc.maxEntries {
			m = MetricCollectors{
				counter:   mc.counter.WithLabelValues(method, "/_overflow", statusStr),
				histogram: mc.histogram.WithLabelValues(method, "/_overflow"),
			}
			return
		}
		if casBetweenLoadAndCompareHook != nil {
			casBetweenLoadAndCompareHook()
		}
		if mc.size.CompareAndSwap(cur, cur+1) {
			break
		}
	}

	// Slot reserved. Try to store
	entry := &metricCacheEntry{
		mc: MetricCollectors{
			counter:   mc.counter.WithLabelValues(method, route, statusStr),
			histogram: mc.histogram.WithLabelValues(method, route),
		},
	}
	entry.lastUsed.Store(nowUnix)

	if casBetweenReserveAndStoreHook != nil {
		casBetweenReserveAndStoreHook(key)
	}
	actual, loaded := mc.cache.LoadOrStore(key, entry)
	if loaded {
		mc.size.Add(-1)
		loadedEntry := actual.(*metricCacheEntry)
		loadedEntry.lastUsed.Store(nowUnix)
		m = loadedEntry.mc
		return
	}

	m = entry.mc
	return
}

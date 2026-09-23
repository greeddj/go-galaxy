package metrics

import (
	"encoding/json"
	"sync"
	"testing"
)

// TestCountersNilReceiverSafe asserts every Counters method tolerates a nil
// receiver without panicking, so a caller never needs a guard before calling
// through a *Counters obtained from, say, a zero-valued Infra in a test.
func TestCountersNilReceiverSafe(t *testing.T) {
	t.Parallel()
	var c *Counters

	c.AddCacheHit()
	c.AddCacheMiss()
	c.AddBytesDownloaded(42)

	got := c.Totals()
	want := Totals{}
	if got != want {
		t.Fatalf("nil Counters.Totals() = %+v, want %+v", got, want)
	}
}

// TestCountersFreshIsZero asserts a newly constructed Counters reports all
// zero totals before any Add* call.
func TestCountersFreshIsZero(t *testing.T) {
	t.Parallel()
	c := &Counters{}
	got := c.Totals()
	want := Totals{}
	if got != want {
		t.Fatalf("fresh Counters.Totals() = %+v, want %+v", got, want)
	}
}

// TestCountersAddBytesDownloadedIgnoresNonPositive asserts AddBytesDownloaded
// silently drops n <= 0, since there is nothing to add and a negative value
// would only be a caller bug, not something worth accumulating as-is.
func TestCountersAddBytesDownloadedIgnoresNonPositive(t *testing.T) {
	t.Parallel()
	c := &Counters{}
	c.AddBytesDownloaded(0)
	c.AddBytesDownloaded(-10)
	if got := c.Totals().BytesDownloaded; got != 0 {
		t.Fatalf("BytesDownloaded = %d, want 0 after only non-positive adds", got)
	}
}

// TestCountersConcurrentAdds pins that concurrent adds from many goroutines
// lose no update, as the install, prefetch and warm pools sharing one Counters
// require; it is meaningful under -race.
func TestCountersConcurrentAdds(t *testing.T) {
	t.Parallel()
	const goroutines = 50
	const addsPerGoroutine = 200
	const bytesPerAdd = 7

	c := &Counters{}
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range addsPerGoroutine {
				c.AddCacheHit()
				c.AddCacheMiss()
				c.AddBytesDownloaded(bytesPerAdd)
			}
		})
	}
	wg.Wait()

	want := Totals{
		CacheHits:       goroutines * addsPerGoroutine,
		CacheMisses:     goroutines * addsPerGoroutine,
		BytesDownloaded: goroutines * addsPerGoroutine * bytesPerAdd,
	}
	if got := c.Totals(); got != want {
		t.Fatalf("Totals() after concurrent adds = %+v, want %+v", got, want)
	}
}

// TestReportJSONIncludesZeroCounters pins that cache_hits, cache_misses and
// bytes_downloaded marshal as an explicit 0, never omitted, so a consumer can
// tell zero apart from a report that predates them.
func TestReportJSONIncludesZeroCounters(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(Report{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"cache_hits", "cache_misses", "bytes_downloaded"} {
		val, ok := raw[key]
		if !ok {
			t.Fatalf("marshaled Report missing key %q, want present with value 0", key)
		}
		if val != float64(0) {
			t.Fatalf("marshaled Report[%q] = %v, want 0", key, val)
		}
	}
}

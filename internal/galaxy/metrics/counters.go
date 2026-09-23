package metrics

import "sync/atomic"

// Counters accumulates one run's artifact-cache tallies. Its methods are safe
// for concurrent use and nil-receiver-safe; infra.New allocates a fresh set per
// run, so a Report's totals describe exactly the run that produced them.
type Counters struct {
	hits            atomic.Int64
	misses          atomic.Int64
	bytesDownloaded atomic.Int64
}

// Totals is a point-in-time read of a Counters.
type Totals struct {
	BytesDownloaded int64
	CacheHits       int64
	CacheMisses     int64
}

// AddCacheHit records one artifact served from the artifact cache, counted once
// per successful ArtifactStore.Fetch rather than per collection.
func (c *Counters) AddCacheHit() {
	if c == nil {
		return
	}
	c.hits.Add(1)
}

// AddCacheMiss records one artifact acquired from the origin, counted once per
// retry-bounded acquisition, so a retried transient failure is one miss.
func (c *Counters) AddCacheMiss() {
	if c == nil {
		return
	}
	c.misses.Add(1)
}

// AddBytesDownloaded records n bytes read from an origin's artifact body, per
// attempt including a failed, retried one; a cache hit, S3 included, adds
// nothing. n <= 0 is ignored.
func (c *Counters) AddBytesDownloaded(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.bytesDownloaded.Add(n)
}

// Totals returns a point-in-time snapshot of c. A nil receiver reads as an
// all-zero Totals, matching the nil-receiver-safety of the Add* methods.
func (c *Counters) Totals() Totals {
	if c == nil {
		return Totals{}
	}
	return Totals{
		CacheHits:       c.hits.Load(),
		CacheMisses:     c.misses.Load(),
		BytesDownloaded: c.bytesDownloaded.Load(),
	}
}

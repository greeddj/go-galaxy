package cache

import (
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Policy controls cache read/write behavior and TTL.
type Policy struct {
	Read  bool
	Write bool
	TTL   time.Duration
}

// Options exposes cache-related flags used to derive a Policy.
type Options interface {
	IsNoCache() bool
	IsRefresh() bool
	IsOffline() bool
}

// PolicyForConstraint builds a cache policy from options and constraints.
// Offline, the cache is the only source of truth: no TTL and no writes, so a
// miss surfaces as an error rather than a network attempt.
func PolicyForConstraint(opts Options, exact bool) Policy {
	if opts == nil {
		return Policy{Read: true, Write: true}
	}
	if opts.IsOffline() {
		return Policy{Read: true, Write: false}
	}
	if opts.IsNoCache() {
		return Policy{}
	}
	if !exact {
		if opts.IsRefresh() {
			return Policy{Write: true, TTL: helpers.CacheLatestMetadataTTL}
		}
		return Policy{Read: true, Write: true, TTL: helpers.CacheLatestMetadataTTL}
	}
	return Policy{Read: true, Write: true}
}

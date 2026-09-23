// Package metrics serializes a small JSON report describing the most
// recent install/warm run, intended for ingestion by CI dashboards.
package metrics

import (
	"encoding/json"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Report captures a single run's outcome. The three cache counters are
// artifact-level and carry no omitempty, so a consumer can tell zero from a
// report that predates them; Frozen reports whether --frozen was honored.
type Report struct {
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      time.Time     `json:"finished_at"`
	Command         string        `json:"command"`
	Server          string        `json:"server,omitempty"`
	LockfilePath    string        `json:"lockfile,omitempty"`
	LockfileHash    string        `json:"lockfile_hash,omitempty"`
	Duration        time.Duration `json:"duration_ns"`
	CacheHits       int64         `json:"cache_hits"`
	CacheMisses     int64         `json:"cache_misses"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	Collections     int           `json:"collections"`
	Roles           int           `json:"roles"`
	Failures        int           `json:"failures"`
	Frozen          bool          `json:"frozen,omitempty"`
	Offline         bool          `json:"offline,omitempty"`
}

// Write marshals r to path atomically through helpers.WriteFileAtomic, so a
// planted symlink is replaced rather than followed; an empty path is a no-op,
// letting callers pass cfg.MetricsFile unconditionally.
func Write(path string, r Report) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return helpers.WriteFileAtomic(path, data)
}

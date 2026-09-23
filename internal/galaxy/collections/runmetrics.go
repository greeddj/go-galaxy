package collections

import (
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/metrics"
)

// runCounts is what a run was about: how many collections and roles its
// plan held and how many of either failed.
type runCounts struct {
	Collections int
	Roles       int
	Failures    int
}

// describe renders "N collections" or "N collections, M roles".
func (c runCounts) describe() string {
	if c.Roles == 0 {
		return fmt.Sprintf("%d collections", c.Collections)
	}
	return fmt.Sprintf("%d collections, %d roles", c.Collections, c.Roles)
}

// writeRunMetrics best-effort writes the metrics report. frozen is what the
// run honored, not what was configured; on a failed install, in-flight
// prefetch workers make the artifact counters a lower bound.
func writeRunMetrics(
	cfg *config.Config,
	runtime *infra.Infra,
	command string,
	start time.Time,
	counts runCounts,
	frozen bool,
) {
	if cfg == nil || cfg.MetricsFile == "" {
		return
	}
	// A report cannot mark a dry run, and would pair the on-disk lockfile's
	// hash with a fresh resolve's counts, so a dry run writes none.
	if cfg.DryRun {
		runtime.Output.Warnf("--dry-run: skipping metrics report to %s", cfg.MetricsFile)
		return
	}
	now := time.Now()
	totals := runtime.Metrics.Totals()
	report := metrics.Report{
		Command:         command,
		StartedAt:       start.UTC(),
		FinishedAt:      now.UTC(),
		Duration:        now.Sub(start),
		CacheHits:       totals.CacheHits,
		CacheMisses:     totals.CacheMisses,
		BytesDownloaded: totals.BytesDownloaded,
		Collections:     counts.Collections,
		Roles:           counts.Roles,
		Failures:        counts.Failures,
		Server:          cfg.Server,
		Frozen:          frozen,
		Offline:         cfg.Offline,
		LockfilePath:    lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile),
		LockfileHash:    tryLockfileHash(cfg),
	}
	if err := metrics.Write(cfg.MetricsFile, report); err != nil {
		runtime.Output.Warnf("Failed to write metrics %s: %v", cfg.MetricsFile, err)
	}
}

func tryLockfileHash(cfg *config.Config) string {
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		return ""
	}
	hash, err := lf.Hash()
	if err != nil {
		return ""
	}
	return hash
}

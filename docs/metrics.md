# Metrics

Pass `--metrics-file path/to/run.json` to install/warm/lock/outdated to emit a
JSON report suitable for CI dashboards:

```json
{
  "started_at":       "2026-04-28T10:00:00Z",
  "finished_at":      "2026-04-28T10:00:08Z",
  "command":          "install",
  "server":           "https://galaxy.ansible.com",
  "lockfile":         "galaxy.lock",
  "lockfile_hash":    "<sha256-hex>",
  "duration_ns":      8123456789,
  "cache_hits":       12,
  "cache_misses":     5,
  "bytes_downloaded": 4831201,
  "collections":      17,
  "roles":            3,
  "failures":         0,
  "frozen":           true
}
```

The report is written atomically (temp file plus rename), so a consumer never
reads a partial JSON, and a symlink at the operator-specified path is replaced
rather than followed.

The report is written whenever a run reaches its finalize step - including a
run that failed to install some collections, a run whose snapshot save itself
failed, and a `lock --frozen` run that found drift - and is not written when
the run aborts earlier (unreadable `requirements.yml`, a resolution failure,
or a missing or unloadable lockfile). A `--dry-run` run is the one exception on
the other side: it reaches finalize and still writes nothing, because the report
carries no field that would distinguish a preview from a real run. All four
commands suppress it and print a stderr warning naming the path that was
skipped - see [--dry-run](cli.md#--dry-run). Its existence is therefore not a success
signal: gate automation on the process exit code (see
[Exit codes](exit-codes.md)), never
on whether the metrics file exists or looks clean. This matters most for
`lock`: `failures` is always `0` in a `lock` report, so the report carries no
failure signal at all for that command, and a `lock` run whose snapshot save
failed, or whose `--frozen` gate found drift, still leaves a clean-looking
report next to a nonzero exit code. `frozen` is `true` exactly when the run
honored `--frozen`, for every command that reads the flag, `lock` included:
for `install`/`warm` that means resolving from the lockfile, and for `lock`
it means gating the fresh resolve against the lockfile instead of overwriting
it - not merely whether the flag was passed. `outdated` never honors
`--frozen`, so its report always omits `frozen`, whatever the flag or
`$GO_GALAXY_FROZEN` said.

`offline` asks a different question than `frozen` does. It does not report
whether anything was honored during the run: `--offline` (or
`$GO_GALAXY_OFFLINE`) configures the HTTP transport for the whole command, so
the field simply mirrors the flag as configured, identically for
`install`/`warm`/`lock`/`outdated`. Like `frozen`, it carries `omitempty` and is
therefore absent from the JSON whenever it is false - which is why the example
above, an ordinary networked run, does not show it at all. A dashboard reading
these reports should treat a missing `offline` as false rather than as unknown.

`server`, `lockfile` and `lockfile_hash` carry `omitempty` too, and in practice
only the last of the three goes missing. `lockfile_hash` is the hash of the
lockfile at the resolved path, read fresh off disk, and it is omitted whenever
that file does not exist or fails to load - so an `install` or `warm` run in a
project that has never run `lock` emits a report with no `lockfile_hash` key at
all. That is not a signal about the run: read its absence as "there was no
lockfile to hash", never as a failure. The three artifact counters,
`cache_hits`, `cache_misses` and `bytes_downloaded`, carry no `omitempty` and
are always present, so a `0` is an explicit zero and a report without them was
written by a binary that predates them.

A collection built from a git source counts like any other artifact, except
that a cold run moves both cache counters for it: discovery counts a miss
when it commits the built artifact to the cache, and the install phase of the
same run then serves that artifact from the cache and counts a hit. The pack
bytes written to disk for it count as `bytes_downloaded` (the pack, not the
artifact built from it, is what crossed the wire). A cold `install` of one git
collection that depends on one Galaxy collection therefore reports
`cache_misses` 2 and `cache_hits` 1, with `bytes_downloaded` equal to the pack
bytes plus the Galaxy tarball. A later run that installs it from the cache is
one more hit. Under `--no-cache` the build is handed straight to the install
phase, so neither a miss nor a hit is counted for it, only its pack bytes.

`roles` is the number of roles in the run's plan - the `roles:` entries plus
the dependencies discovered through them, for `install`/`warm`, the roles the
written lockfile holds for `lock`, and the role entries checked for
`outdated` - and is `0`, never absent, for a run without roles. `failures`
counts both kinds together: a failed role and a failed collection each add
one. A role's artifact counts in the three artifact counters as a git
collection's does. The commit at discovery is a miss, the bytes fetched for it
(the pack of a git or Galaxy role, the tarball of a url role) are
`bytes_downloaded`, and the `install` or `warm` phase of the same run serves it
from the cache and counts a hit, so a cold run counts both; a later run that
installs it from the cache counts one more hit. Under `--no-cache` the build
goes straight to the install phase and neither cache counter moves for the
role, only `bytes_downloaded`. So `cache_hits + cache_misses` counts collection
and role acquisitions alike.

`cache_hits`, `cache_misses`, and `bytes_downloaded` are artifact-level counters,
not collection-level: a hit is one artifact served from the artifact cache and a
miss is one artifact fetched from the origin, so `cache_hits + cache_misses`
counts artifact acquisitions rather than collections. That sum can exceed
`collections` - a cold git collection counts twice, as above, and the bounded
[evict-and-refetch recovery](architecture.md#bounded-recovery) path can make
one collection contribute both a hit and a miss - and it can also fall below
`collections`, since a collection whose install is skipped touches no artifact
at all. A hit is counted only once the cache has served the artifact, so that
recovery path counts differently per backend. The local cache does not verify
a tarball on read: a corrupt one is served (a hit), fails its digest or
extraction check, and is refetched (a miss). The S3 cache checks an object's
bytes against its recorded sha256 as part of the read, so an object whose
bytes disagree with it fails before it is served and its recovery adds only
the miss; one that passes that check and fails a later one counts a hit and a
miss, as on the local cache. A cache hit always contributes zero bytes to
`bytes_downloaded`, including an S3 cache hit: that object transfer is a real
network round trip to the cache backend, but it is not artifact-download
traffic, so it is deliberately excluded. The
`lock` command never downloads a Galaxy artifact, so for a file of Galaxy
collections its report has `cache_hits`, `cache_misses`, and
`bytes_downloaded` at `0`; a git collection or a role it locks is fetched and
built during its resolve, and counts as above.

`cache_misses` and `bytes_downloaded` are counted on different units, so
neither can be derived from the other. For a Galaxy artifact, a miss is
counted once per acquisition, after the retries succeed: a download retried
after a transient failure is one miss, and one that fails every attempt
records none. `bytes_downloaded` is added per attempt, a failed attempt's
partial body included, so a download that stalled or failed still reports
the body bytes it read, once for each attempt.

On a failed `install`, the three artifact counters are a lower bound rather than an
exact count. Installation stops after the first install level with a failure,
but the [prefetcher](architecture.md#prefetch-and-handoff) may already have
workers in flight for a later level, and the report is built before those
workers are canceled and joined, so a late miss and its bytes can land after
the report was written. On a successful run every prefetched artifact has
been consumed first, so the totals are complete.

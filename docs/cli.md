# CLI reference

This page lists what each command and option does; [Command flows](commands.md)
draws how each command runs and which flags change its path.

## Usage

```bash
go-galaxy install -r requirements.yml -p ./collections --roles-path ./roles
```

Clean unreachable collections and roles:

```bash
go-galaxy cleanup
```

### Commands

Running `go-galaxy` with no command runs `install`, so a bare invocation
performs a full install rather than printing help. The same happens from the
first flag the root command does not define: that flag and every word after
it go to `install`, so `go-galaxy -r requirements.yml -p ./collections` is
`go-galaxy install -r requirements.yml -p ./collections`. A command name after
such a flag is no longer read as a command: `go-galaxy -r requirements.yml
lock` hands `lock` to `install` as an argument, which is refused as a usage
error (`2`), and a flag nobody defines is reported against `install`'s own
flag set.

No command but `explain` takes a positional argument: collections and roles
are named in the requirements file, never on the command line, and an argument
a command does not take is refused as a usage error (`2`) before anything
runs, naming every argument it refused. That includes a first word that names
no command, which reaches `install` as an argument - `go-galaxy collection
install ns.name` is refused rather than installing the requirements file with
`ns.name` silently dropped, and so is `go-galaxy help`, since there is no help
command - help is `--help`. `explain` takes exactly one, and run with none it
is refused as a usage error (`2`) the same way.

- `install` (`i`) - install the collections and the roles of `requirements.yml`, as `ansible-galaxy install -r` does; there is no separate role subcommand. Collections go under `--download-path`, roles under `--roles-path` (see [install options](#install-options)). Roles install after the collections and only when every collection level succeeded, so a collection failure never leaves roles half-installed against a broken tree; a run with no `roles:` entries never creates the roles directory. Each role is reported on its own line (`Installed: role <name> == <version>`, `Skipping install, already installed: role <name>@<version>`, `Failed: role <name> == <version> error: ...`), and the completion line counts roles only when the run had any, so a collections-only run reads as it always did.
- `lock` (`l`) - resolve and write `galaxy.lock` for reproducible CI. See [lock](#lock) below for its `--frozen` drift gate and its `--dry-run` preview.
- `warm` (`w`) - populate the artifact + extracted caches without installing (for CI image bake). A role is warmed like a collection - its artifact into the artifact cache and its tree into the extracted store, recorded in the warmed set under `role:<name>@<version>` so a role and a collection sharing a `name@version` never overwrite each other's entry - and reported as `Cached: role <name> == <version>`, the shape a collection's own `Cached: <namespace>.<name> == <version>` line takes. No roles directory is touched. Before it resolves a role or warms a collection, `warm` runs the checks `install` runs on the resolved collections: a dependency cycle, or a requirements root the resolution left out, fails the run with the resolution code (`3`), `--dry-run` included, rather than baking a cache for an install that would refuse the same graph. Requires a cache: `--no-cache` is rejected as a usage error rather than downloading everything and discarding it. A warmed collection's extracted tree is protected from `cleanup` for 30 days after its last warm, so a machine that warms and then stops warming eventually reclaims the space. Under `--dry-run`, `warm` reports per collection whether it is already warm or would be warmed, downloads no Galaxy artifact, and writes no warmed entry; it still rejects `--no-cache` as a usage error regardless of `--dry-run`, since `--no-cache` leaves warm nothing to do either way - see [install options](#install-options) for the full `--dry-run` semantics.
- `hash` (`h`) - print a deterministic cache key (`sha256:…`) for use as a CI cache key.
- `tree` (`t`) - print the resolved dependency tree from the lockfile, rooted at the entries of `requirements.yml`. A git entry prints its repository, subdir and commit beside its version, and a git requirement's roots are whatever entries the lockfile holds from that repository under its subdir. A url entry, collection or role, prints `(url <url> sha256:<first 12 hex digits of its pin>)` beside its version, and a url requirement the lockfile holds nothing for prints as its locator `url+<url>#` marked `(missing in lockfile)`. When the lockfile or the requirements file has roles, a second group follows under a `roles:` header, one tree per `roles:` root through the dependencies the lockfile recorded: a git role prints as `<name> <version> (git <repository> @<commit>)`, a Galaxy role as `<name> <version> (galaxy <owner.role> via <repository> @<commit>)`, and a root the lockfile lacks as `(missing in lockfile)`; nothing is printed for a file without roles. Within one root's tree, collections and roles alike, a dependency that tree has already printed is shown again as `<name> <version> (*)` and not expanded; each root starts fresh, so an entry two roots share is printed in full under each. It needs both files, not just the lockfile: a missing lockfile fails with the lockfile code (`6`), while a requirements file that is missing, cannot be read or is not YAML fails with the usage code (`2`), since the roots to walk from are read from it. Only lockfile entries reachable from a requirements root are printed, so an entry no root reaches is silently omitted, and a root with no lockfile entry prints as `(missing in lockfile)`.
- `explain` (`why`) - takes `<namespace.name>` or a role's install name or Galaxy name (`owner.role`); prints the locked version, source, and sha256 (for a git entry: type, source, ref, commit and subdir; for a role: `role <name> <version>`, then its type and source and the pin its type carries - galaxy, repository, ref and commit for a Galaxy role, ref and commit for a git role, sha256 for a url role), what requires it, and what it depends on. A name that is both a collection and a role prints both sections, the collection first; a name that is neither fails with `collection or role not found in lockfile` and exits with the generic code (`1`), which keeps it apart from a missing or invalid lockfile (`6`) and from no name or more than one (`2`). Every line comes from the lockfile except one: the `requirements.yml (root)` entry under `required by` is decided by the requirements file, and unlike `tree`, `explain` discards that file's load error rather than failing on it - so with the requirements file missing or unparseable a genuine top-level collection prints as an orphan with no parents instead.
- `outdated` (`o`) - compare each entry the project is running against the latest version on its Galaxy server, or, for a git entry, the locked commit against what its ref points at now; a url entry is reported as current by construction, since its content-addressed pin has no version feed to compare against (drift is `--refresh` and `--frozen` territory). The entries come from the lockfile, or - when there is none - from the installed collections tree at `collections_path`, so a project that never locks can still be checked; see [outdated](#outdated) for what that fallback covers. A locked role is reported on a line whose name carries a `role` prefix: a git role is compared by commit like a git collection; a Galaxy role is compared against the highest tag the v1 API lists today, by name rather than by semver order (role tags are not held to semver), and a role the server lists no tags for is compared by its default branch's commit. See [outdated](#outdated) below for what it does not do: no cache backend, no lock, no cached answer.
- `cleanup` (`c`) - remove unused cached collections and roles across projects.

#### lock

- `lock` (`l`) - resolve and write `galaxy.lock` for reproducible CI. A `roles:` list is resolved in the same run - a git or Galaxy role's repository fetched and its commit pinned, a url role's tarball downloaded and pinned by its sha256, see [The lockfile](architecture.md#the-lockfile) - and the written-to line counts roles when the file has any. Under `--frozen`, `lock` becomes a drift gate instead of a writer: it still resolves fresh (`lock` always does), but compares that fresh resolve against the lockfile already on disk and fails the run instead of overwriting the file when they differ, exiting with the lockfile exit code (`6`). The gate mirrors `lock`'s own resolution per the exact flags in effect - what it compares against is whatever `lock --<those flags>` would write - so `lock --frozen` alone reuses a cached resolve when `requirements.yml` is unchanged, and a version merely published upstream is not drift by itself: it gates the requirements-to-lockfile relationship, not upstream publication. Add `--refresh` (`lock --frozen --refresh`) to gate upstream publication too: `--refresh` makes the fresh resolve reach the live servers instead of reusing the cached one, so a newer version published upstream with `requirements.yml` unchanged now shows up as drift. A missing lockfile and one that exists but cannot be loaded each fail with their own distinct error rather than being reported as drift. Role drift counts the same way: a role added, removed, or repinned (a different tag, ref, commit, repository, type or dependency list) fails the gate, reported as `Would add role: <name>@<version>`, `Would update role: <name> (<field> <from> -> <to>; ...)` or `Would remove role: <name>@<version>` plus a `roles:` summary line, both printed only when the file or the diff has a role. Under `--dry-run`, `lock` diffs a fresh resolve against whatever lockfile is already on disk and reports what would change, without writing a lockfile; `--frozen` and `--dry-run` compose (both suppress the write, `--frozen` supplies the stricter verdict) - see [install options](#install-options) for the full `--dry-run` semantics.

#### outdated

- `outdated` (`o`) - compare each entry the project is running against the latest version on its Galaxy server; requires network and is refused under `--offline`. The report prints a line only for an entry that needs attention - `Outdated: <name> <current> -> <latest>` on stdout, `Lookup failed:` on stderr - followed by a summary line that counts every entry, up-to-date ones included. `--verbose` adds a line for each up-to-date entry too, `Up to date: <name> == <version>`, with the version dimmed the way `install` prints it. A git entry is compared by commit: one advertisement round trip asks the remote what the locked ref points at now, and the report line names both commits in full; an entry locked from a commit ref is up to date by definition and costs no round trip, and a url entry likewise - its pin is the content itself, so there is nothing to ask anyone. It deliberately opens no cache backend, so it never takes the exclusive cache lock (it can run alongside an `install` or `warm` against the same cache) and every version it reports is a live answer rather than a cached one. That is also why `--no-cache`, `--refresh`, `--clear-cache`, `--cache-dir` and `--s3-bucket` have nothing to act on; `--no-deps` likewise, since it resolves no dependency graph, and `--frozen` likewise, since the servers are always asked for the latest. Setting any of those (except `--cache-dir`, which cannot be told apart from its default) prints one stderr warning naming them; the other `--s3-*` flags are not individually named, since none of them do anything for any command unless `--s3-bucket` is also set. `--s3-bucket` is still checked the way every command that registers the S3 flags checks it: set without both `--s3-access-key` and `--s3-secret-key`, or set together with `--offline`, it is refused as a configuration error (`2`) before any warning, lookup or `--offline` refusal. `--download-workers` is accepted and ignored as well: each of `outdated`'s lookup pools, for collections and for roles, is sized by `--workers`, so that flag, not `--download-workers`, bounds how many requests `outdated` sends to a server at once. The warning does not name it, since, like `--cache-dir`, it cannot be told apart from its default. It honors `--metrics-file`: the report's `collections` and `roles` are the number of entries of each kind checked and `failures` is the number of lookups that failed, while `frozen` is always absent, since no `outdated` run ever honors that flag. A Galaxy role's lookup asks the Galaxy server the lockfile entry names (`source`) through its v1 role API, so a lockfile locked against galaxy.ansible.com is checked there even when the run's own server list has changed; a git role costs one advertisement round trip, as a git collection does. A run in which any lookup failed exits with the network code (`4`), except that a lookup refused because the Galaxy server supplied a metadata URL embedding a credential in its userinfo exits with the install code (`5`): that refusal is deterministic, so a retry would only repeat it. Another failure shape is classified more specifically, and at load rather than at lookup: a lockfile entry's `name` must be `<namespace>.<name>` with each half matching `^[a-z][a-z0-9_]*$` - the alphabet galaxy.ansible.com and Automation Hub themselves accept - and a lockfile carrying anything else is refused as invalid with the lockfile code (`6`) before a single request is made. Previously only the two-part split was checked, so a name carrying characters a URL cannot contain loaded fine and failed later while the request was being built, reported as a network failure (`4`) that no retry could repair.

  With no lockfile, `outdated` reads the installed collections tree instead of failing, the way `hash` falls back to `requirements.yml`. The tree is the one `--download-path` names - `[defaults] collections_path` or `ANSIBLE_COLLECTIONS_PATH` when they set it - and the versions come from the `<namespace>.<name>-<version>.info/GALAXY.yml` sidecar ansible's own layout puts beside each install, so a tree `ansible-galaxy` installed reads the same as one this tool installed. Each collection is asked about at the server its own sidecar records, falling back to the run's configured server when the sidecar names none; a sidecar naming a version the collection's `MANIFEST.json` does not is a leftover from an earlier install and is ignored, so the report names what is running rather than what once was. Two things the tree cannot answer are named on stderr instead of being counted as current: a collection installed from git, whose sidecar records the repository and the commit but no ref to compare against, and every installed role, since ansible's `meta/.galaxy_install_info` records a version and an install date and no source at all. Run `lock` to cover either. A run with no lockfile *and* no tree (`--download-path` or its `ansible_collections` directory does not exist) still exits `6` and names both paths - the fallback widens where the current side may come from, and adds no exit class of its own. A tree that exists but cannot be opened or listed (permission denied, or a file where the directory should be) is not treated as absent: its error is reported as it is and exits with the generic code (`1`). An `ansible_collections` directory that exists and is empty reports no entries and exits `0`. This is also why `--download-path` is not among the flags the warning above names: with no lockfile it is what the command reads.

### Global options

- `--help, -h` - every command has its own: it prints that command's help and
  exits `0`. Before any command word it reads the next word as a help topic:
  `go-galaxy -h hash` prints `hash`'s help, and a word that names no command is
  refused with `No help topic` (exit `2`).
- `--version, -v` - root only: `go-galaxy --version` prints the version and
  exits `0`. After a command word `--version` is an undefined flag (exit `2`),
  and `-v` is accepted and ignored.

The four options every command inherits from the root (`--verbose`, `--quiet`,
`--dry-run`, `--cache-dir`) are listed under [install options](#install-options).

### install options

`warm` takes this entire set, not just `--dry-run`: `install` and `warm` each
register the identical collection flag set plus the signature-verification
flags plus the S3 flags. `lock` and `outdated` register the collection flag
set plus the S3 flags too, but never the signature-verification flags -
neither command verifies a signature, so mounting those four on them would
advertise a setting that could never do anything; see [Signature
verification](signatures.md#signature-verification) for what each of the four does on the
two commands that do register them. Every one of the four commands sits on
top of the four global options (`--verbose`, `--quiet`, `--dry-run`,
`--cache-dir`) the root command declares once for every subcommand. A flag
accepted by a command that has nothing to act on is accepted, not rejected -
`outdated`, which opens no cache backend, warns on stderr about the
cache-shaped flags rather than failing (see its entry under
[Commands](#commands)). `cleanup` is the exception and takes only the four
global options plus the S3 flags, listed separately under
[cleanup options](#cleanup-options); `hash`, `tree` and `explain` take a
two-flag set of their own, listed under
[hash / tree / explain options](#hash--tree--explain-options).

- `--verbose` - verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` (`$GO_GALAXY_QUIET`) - suppress progress and log lines; results, warnings and
  errors still print. Ignored when `--verbose` is also set.
- `--dry-run` (`$GO_GALAXY_DRY_RUN`) - report what a command would do, without doing it and
  without leaving anything behind that a later run would read as real. See
  [--dry-run](#--dry-run) at the end of this section for what each command suppresses.
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--server` (`$GO_GALAXY_SERVER`) - **Breaking change:** `$ANSIBLE_GALAXY_SERVER`
  is no longer read as a spelling of this flag. It now behaves as `[galaxy] server`
  does, which is what ansible itself does with it: a fallback consulted only when
  no `server_list` and no `--server` apply, rather than an override that collapses
  a configured `server_list` to one anonymous server. A pipeline that exported it
  to force a single server now gets `server_list` instead, and should set
  `$GO_GALAXY_SERVER` (or pass `--server`) to keep the old behavior.
- `--token` (`$GO_GALAXY_TOKEN`) - Galaxy API token for the single effective server;
  an error if a multi-entry `server_list` is in effect, and setting it to the
  empty string clears a previously configured token. What decides that error is
  the count of servers left after precedence rather than the length of
  `server_list` itself, so naming one of its ids with `--server` collapses the
  list to a single server that `--token` may then set. **Breaking change:** also
  refused, and not merely applied, when that single server's URL was sourced
  from an ansible.cfg file rather than from you, and refused the same way when
  that server's certificate verification was disabled by the file rather than
  by you - see [--token](servers-and-auth.md#--token) for the remedy each half needs and why a
  section's own `token` key does not authorize an override either.
- `--timeout` (`$GO_GALAXY_SERVER_TIMEOUT`, `$GO_GALAXY_TIMEOUT`, `$ANSIBLE_GALAXY_SERVER_TIMEOUT`)
  It takes either a bare integer number of seconds (`60`, which is ansible's own form) or a Go
  duration string (`90s`, `1m30s`). When neither the flag nor any of its variables is set, the value
  comes from `server_timeout` in the `[galaxy]` section of `ansible.cfg`, read by the same grammar,
  and only without that from the `30s` default - the order ansible itself applies to this setting.
  Zero, negative and unparseable values are all refused as a usage error exiting `2`
  (`invalid timeout`), whichever of those places supplied them: `--timeout 0` does not mean "no
  timeout", it fails the run before a request is made.
  `--timeout` is a no-progress budget - it bounds the response-header wait and the gap between two
  body reads. It bounds neither total transfer time nor a byte-drip: a server that keeps dribbling a
  few bytes into every idle window counts as making progress on every single read, so it never trips
  this timeout and can drag a download out indefinitely. Every artifact acquisition additionally
  carries a fixed, non-configurable 15-minute ceiling on the whole acquisition - the response, the
  streamed body, extraction running alongside it, the cache commit, and every retry attempt and
  backoff sleep together, not per attempt - which is what actually bounds a slow-drip transfer. This
  ceiling also covers a cache-resident artifact fetched from an S3-backed cache, since that is a full
  artifact body transfer over HTTP too, not a cheap metadata check. An ordinary collection spends
  this budget exactly once: whichever side actually acquires the artifact pays it alone, because the
  other side never touches the network for that collection - either the background prefetcher
  downloads ahead of the install worker and hands off the verified bytes, or, for an artifact that
  was already cache-resident and was therefore never scheduled for prefetch at all, the install
  worker acquires it itself. A second budget is spent only on a failure path: a failed prefetch
  hands the worker nothing, so the worker's own fresh download pays a budget on top of the one the
  failed prefetch already spent, and a cache-hit artifact that fails its pin/hash check or fails to
  extract is evicted and refetched exactly once, spending a second budget for that same collection.
  So 15 minutes bounds an ordinary collection, and 30 minutes bounds one that took either of those
  failure paths. One narrow corner spends a third budget, 45 minutes, by composing them: a prefetch
  that spends a whole budget and fails, then a cache-hit fetch that spends a second and fails its
  read-time integrity check, then the evict-and-refetch that spends a third against the origin.
  Reaching it needs a degraded origin and a damaged cache entry for the same collection in the same
  run, which is why the two-budget figure is the one to plan against.
  On a slow enough link the collection fails closed with `artifact download deadline exceeded` and
  is not installed, rather than hanging or completing arbitrarily late: a maximum-size (4 GiB)
  artifact needs roughly 38 Mbit/s sustained to finish inside the budget, while a real collection
  needs well under 1 Mbit/s.
  This ceiling is not configurable, unlike `--timeout` above: a knob on a safety ceiling is a knob an
  operator would raise in direct response to a truncation, which is exactly how the slow-drip attack
  this closes would succeed. A collection whose download stalls or drips fails that collection and the
  run exits with the install-failure code (`5`); a stall outside the per-collection install path - a
  metadata fetch during resolution, an S3 state-object read - exits with the network code (`4`)
  instead. It is never reported as an interrupt.

  Three more ceilings complete this family, all fixed and non-configurable for the identical reason: a
  knob on a safety ceiling is one an operator raises in response to a truncation, which is how the
  attack succeeds. A single Galaxy metadata request - the response, the size-limited body read, and
  every retry attempt and backoff sleep, as one shared budget - carries a fixed 2-minute ceiling: a
  maximum-size (16 MiB) response needs roughly 1.12 Mbit/s sustained to finish inside it, while the
  largest realistic response (a 10,000-version list, about 1.5 MB) needs only about 0.1 Mbit/s. The
  versions-list paging loop shares a single one of these budgets across every page it fetches, rather
  than spending a fresh one per page, since the server itself controls how many pages a resolve issues.
  A single persisted cache-state operation (loading or saving the snapshot, loading or recording the
  project registry) carries a fixed 60-second ceiling: a maximum-size (256 MiB compressed) state object
  needs roughly 35.8 Mbit/s sustained to finish inside it, while a large real snapshot (16 MiB
  compressed) needs only about 2.2 Mbit/s. This ceiling also protects every other runner sharing an
  S3-backed cache, not just the one that is stalling: these operations run while the backend's
  distributed lock is held, so an unbounded one blocks every other runner against that bucket until it
  gives up waiting for the lock - which is why its budget is tighter than the artifact ceiling above.
  One collection's whole signature phase - every declared and server-offered source it gathers, with
  the public-key check interleaved with each one - carries a fixed 1-minute ceiling: an ordinary
  collection spends it once, and a collection whose manifest chain check, manifest lookup, or
  attribution check fails against a cache-hit artifact is evicted and refetched exactly once (the same
  bounded recovery every artifact acquisition gets), whose retry gathers every one of that collection's
  signature sources again under a second, fresh 1-minute budget - the identical ordinary/failure-path
  split as the artifact ceiling's own 15-/30-minute one above. A bare policy verdict on its own (the
  signatures in hand simply did not satisfy the required count) is never retried and so never spends a
  second budget. This ceiling applies only to a run that verifies signatures at all - see [Signature
  verification](signatures.md#signature-verification); a run with no keyring configured, or with verification
  switched off, gathers nothing and pays nothing toward it.
- `--download-path, -p` (`$GO_GALAXY_COLLECTIONS_PATH`, `$GO_GALAXY_DOWNLOAD_PATH`, `$ANSIBLE_COLLECTIONS_PATH`)
- `--roles-path` (`$GO_GALAXY_ROLES_PATH`, `$ANSIBLE_ROLES_PATH`) - the directory roles install into,
  one `<roles-path>/<name>/` per role; defaults to `.roles`, project-local like `.collections`. The
  flag and its variables outrank `[defaults] roles_path` in `ansible.cfg`, which outranks the default.
  Like `collections_path` it is ansible's `:`-separated search list read as one path: the first entry
  is used and the rest are named in a stderr warning. A roles path equal to the collections path is
  accepted with a warning (roles then sit beside `ansible_collections`; a role named
  `ansible_collections` is refused by the install-name alphabet, so it can never replace that tree).
  Both warnings are printed only when the requirements file carries a non-empty `roles:` block, the
  same condition under which the setting is read at all. The directory is created only when the run
  has a role to install, never on a dry run.
- `--requirements-file, -r`, also spelled `--role-file` (`$GO_GALAXY_REQUIREMENTS_FILE`,
  `$ANSIBLE_GALAXY_REQUIREMENTS_FILE` - a go-galaxy extension, not an ansible option). `--role-file`
  is `ansible-galaxy`'s own name for this flag; it names the same file, which carries the
  `collections:` and the `roles:` lists alike.
- `--ansible-config` (`$GO_GALAXY_ANSIBLE_CONFIG`) - name an `ansible.cfg` explicitly. Both the
  flag and that one variable are strict: the file has to be there, and a path that does not exist
  is a usage error exiting `2`, never a silent fallback. A file that exists but cannot be read to
  the end - permission denied, a directory, a line longer than 64 KiB - exits `2` as well, named
  here or found by discovery. `$ANSIBLE_CONFIG` is not a second source
  for this flag and does not behave like one: it is the first candidate of ansible's own discovery
  order (see [what go-galaxy reads](configuration.md#what-go-galaxy-reads)), so a path that
  does not exist there is not an error at all - discovery just moves on to `./ansible.cfg`,
  `~/.ansible.cfg` and `/etc/ansible/ansible.cfg`, and finding none of them is fine too.
- `--workers` (`$GO_GALAXY_WORKERS`) - number of concurrent workers. Unset, it derives from the CPU
  this process is permitted to use (a container quota, not the node's core count), floored at 2 and
  capped at 16. A value is accepted from `1` up to that permitted CPU count, itself floored at 2 -
  so a 1- or 2-CPU runner still accepts `2`. Outside that range, in either direction, one warning is
  printed to stderr and the derived default is used instead, never a usage error. A value that is
  not an integer at all is a different matter: it fails the flag parse itself and still exits `2`.
  **Breaking change (after v1.0.2):** through v1.0.2, `--workers` (and `$GO_GALAXY_WORKERS`) was
  taken as given: any value at or above `1` was honoured however far it exceeded the machine, and a
  non-positive one silently became the node's core count. Two groups are affected. An operator
  deliberately oversubscribing - `--workers=32` on a 4-CPU runner - no longer gets the pool they
  asked for: it is replaced by the derived default and one warning goes to stderr, so ask for a
  count inside the accepted range instead, or raise the CPU the runner permits if the larger pool
  was the point. And a pipeline branching on a nonzero exit from `--workers=0` now sees that warning
  and a successful run instead: v1.0.2 substituted the core count silently, and a build tracking
  main between releases exited `2` here, but neither happens now - drop the value, it never
  selected a pool. The ceiling is machine-dependent (the CPU this process is permitted to use,
  floored at 2), so one fixed `--workers` value in a shared CI config can be accepted on one runner
  and replaced with a warning on another. Where creating files and directories is slow, a value
  below the derived default can be faster (APFS on a 12-core host peaked at 4 workers); see
  [Two pools, two resources](architecture.md#two-pools-two-resources).
- `--download-workers` (`$GO_GALAXY_DOWNLOAD_WORKERS`) - number of concurrent artifact downloads
  and cache presence probes, separate from `--workers`: `--workers` bounds extraction, which is
  local work bound mostly by filesystem metadata writes and partly by CPU (an install or warm
  worker unpacks a tree in the same goroutine that downloaded it), while `--download-workers`
  bounds downloads and cache probes, which are network-bound (a HEAD probe or a streamed GET into
  a temp file, never an extraction). Unset, it derives from that same
  permitted CPU count (4× per CPU) with a floor of 8 and a ceiling of 32, so a low-CPU CI runner
  still gets meaningful download concurrency and a high-CPU one does not oversubscribe the HTTP
  connection pool. A non-positive value falls back to that same default rather than erroring.
  **Breaking change (after v1.0.2):** through v1.0.2, the concurrency of artifact downloads and
  cache presence probes followed `--workers` (one per CPU by default), so a 4-core CI runner issued at
  most 4 concurrent requests to the configured Galaxy server. It now follows `--download-workers`'s
  own, larger default instead, so that same 4-core runner issues up to 16 concurrent requests. This
  matters to operators of rate-limited Automation Hub instances, or any Galaxy server enforcing a
  per-client request limit: set `--download-workers` (or `$GO_GALAXY_DOWNLOAD_WORKERS`) explicitly
  to keep the old, CPU-derived figure, or lower, if the new default triggers throttling (`outdated`
  follows `--workers` instead).
- `--no-cache` (`$GO_GALAXY_NO_CACHE`)
- `--refresh` (`$GO_GALAXY_REFRESH`) - re-resolve against the configured Galaxy servers instead of reusing
  cached metadata or the previous resolution. It bypasses exactly the cached answers that name a collection
  without naming a version - which versions exist, which is highest, and which versions the last run picked
  for these requirements - never an answer that already names an exact version: that version's metadata,
  its dependency map, its artifact bytes, and its extracted tree are all still served from cache, so a
  refreshed re-resolve that lands on the same version downloads nothing new. `--offline` outranks
  `--refresh` - cached state is the only source of truth offline, so there is nothing left to re-resolve
  against - and the run warns once rather than silently dropping the flag. `--refresh` has no effect under
  `--frozen` on `install`/`warm`, since neither ever resolves against the network in the first place; on
  `lock --frozen` it does have an effect - see the `lock` command entry above. For a git source a branch or
  tag is the version-free answer: `--refresh` asks the remote again what the ref points at, keeps the
  recorded commit when it has not moved and the artifacts are still cached, and fetches the new commit
  otherwise; a full commit ref is its own answer and is never re-advertised. A role follows the same
  rule for its ref, and a Galaxy role additionally asks the v1 API again which tag is highest. Unlike a collection
  pinned to an exact version, a role naming an explicit tag is re-asked too, since a tag is still a
  name the repository can move; the recorded commit is kept when it has not.
- `--clear-cache` (`$GO_GALAXY_CLEAR_CACHE`) - also forgets every recorded git, url and role pin, so the
  next resolve asks each repository and URL, and the Galaxy v1 API, again; the records of installed roles
  survive, as the records of installed collections do.
- `--no-deps` (`$GO_GALAXY_NO_DEPS`) - installs only what `requirements.yml` names. For collections
  it drops every dependency edge from the solve, so only the roots are resolved and installed; for
  roles it stops the dependency walk at the `roles:` entries: nothing a role's `meta/main.yml` or
  `meta/requirements.yml` names is installed. `--no-deps` has no effect under `--frozen` on
  `install`/`warm`: the lockfile is installed whole, dependencies included, as it is written. To
  install without dependencies from a lockfile, write that lockfile with `lock --no-deps`; its
  entries then hold only the roots.
- `--offline` (`$GO_GALAXY_OFFLINE`) - fail on any network access (cached state only). A git source
  replays its recorded pin and its cached artifact, or fails. A role does the same: a git role needs
  its recorded pin, a Galaxy role its recorded v1 answer and the git pin beneath it, a url role its
  recorded pin under the `version:` label it asks for, and each needs the cached artifact, else the
  run fails with the network code (`4`) before anything installs.
  Under `--frozen` the pins come from the lockfile instead, and a role whose artifact is not cached
  fails on its own (see `--frozen` below). The S3 cache is reached over the network, so `--offline`
  together with `--s3-bucket` (either one from its variable included) is refused as a usage error
  (`2`) while the configuration is built, before any cache backend opens, on every command that
  takes both - `install`, `warm`, `lock` and `outdated`. An offline run uses the local cache.
- `--lock-file` (`$GO_GALAXY_LOCK_FILE`)
- `--frozen` (`$GO_GALAXY_FROZEN`) - the lockfile is law, enforced differently by each command it applies to. `install`/`warm` resolve FROM the lockfile instead of the network: no version listing, no metadata fetch, just the pinned entries. A collection entry pinned by a SHA256 (a Galaxy or url collection) is installed only from an artifact whose SHA256 matches that pin, and an installed copy recorded with another SHA256 is installed again rather than kept. A cached artifact that does not match is evicted and downloaded again once from its origin (not under `--offline`, which keeps the only local copy); a downloaded artifact that does not match, or a cached one under `--offline`, fails that collection, and the run fails with the integrity code (`7`) once the work in flight has finished: `warm` finishes every other collection, and `install` finishes the current dependency level and starts no later one (see [Bounded recovery](architecture.md#bounded-recovery)). `lock` cannot resolve from a file it is about to write, so it instead resolves fresh - exactly as an unfrozen `lock` run does, including reusing a cached resolve - and COMPARES the result against the lockfile already on disk, failing the run rather than overwriting the file on any difference; `lock --frozen` is therefore not a network-free path the way install/warm `--frozen` is. See the `lock` command entry above for the comparison's exact contract. A git entry is pinned by its commit rather than by a SHA256 (see [The lockfile](architecture.md#the-lockfile)): a frozen `install`/`warm` installs it from the cache, and a cache that lacks its artifact fetches exactly that commit and rebuilds it. A git or Galaxy role entry is pinned the same way: with the artifact cached, a frozen run touches no network for it; on a cache miss the role is fetched by its pinned commit from the lockfile's repository, and a remote that serves a different commit for it fails the run with the integrity code (`7`). A url role entry is pinned by its origin bytes' SHA256, like a url collection, and its cached artifact, repacked from those bytes, is keyed by that pin: with it cached a frozen run touches no network for the role, and on a cache miss the URL is downloaded again and must hash to the pin, or the run fails with the integrity code (`7`). Under `--offline` a cache miss, collection or role, fails that entry, and the run exits with the install-failure code (`5`) like any other per-item failure. A `roles:` entry that drifted from its locked line - a different Galaxy version, a different repository or ref, a missing entry - is a lockfile mismatch (`6`).
- `--metrics-file` (`$GO_GALAXY_METRICS_FILE`) - emit JSON run report

Signature verification options (`install` and `warm` only, their environment
variables included; see [Signature
verification](signatures.md#signature-verification) for what each one does):

- `--keyring` (`$GO_GALAXY_KEYRING`, `$ANSIBLE_GALAXY_GPG_KEYRING`) - path to the OpenPGP keyring
  collection signatures are verified against. Unset (the default) means no verification at all.
- `--required-valid-signature-count` (`$GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT`,
  `$ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT`) - defaults to `"1"`.
- `--ignore-signature-status-code` (`$GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE`,
  `$ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES`) - repeatable.
- `--disable-gpg-verify` (`$GO_GALAXY_DISABLE_GPG_VERIFY`, `$ANSIBLE_GALAXY_DISABLE_GPG_VERIFY`) -
  skip verification even when a keyring is configured.

S3 cache options (if `--s3-bucket` is set, S3 backend is used):

- `--s3-bucket` (`$GO_GALAXY_S3_BUCKET`) - refused beside `--offline` (exit `2`), since the bucket
  is reached over the network.
- `--s3-region` (`$GO_GALAXY_S3_REGION`)
- `--s3-prefix` (`$GO_GALAXY_S3_PREFIX`)
- `--s3-access-key` (`$GO_GALAXY_S3_ACCESS_KEY`, `$AWS_ACCESS_KEY_ID`)
- `--s3-secret-key` (`$GO_GALAXY_S3_SECRET_KEY`, `$AWS_SECRET_ACCESS_KEY`)
- `--s3-endpoint` (`$GO_GALAXY_S3_ENDPOINT`)
- `--s3-session-token` (`$GO_GALAXY_S3_SESSION_TOKEN`, `$AWS_SESSION_TOKEN`)
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`) - switch to virtual-hosted-style
  addressing (`<bucket>.<endpoint>/<key>`). Path style (`<endpoint>/<bucket>/<key>`) is the default,
  which is what the flag disables.

#### --dry-run

- `--dry-run` (`$GO_GALAXY_DRY_RUN`) - report what `install`, `warm`, or `lock` would do, without
  downloading any Galaxy artifact, creating any install tree or extracted tree, recording any install,
  writing any warmed entry, writing any lockfile, registering the project, honoring
  `--clear-cache`, or writing the metrics report. It still takes the exclusive cache lock. It
  updates the resolve-side metadata caches, but only when a persisted snapshot already existed for
  this cache; against a cache that was never saved before, the run saves nothing and prints a
  stderr warning that the caches it built are discarded - a preview must never leave behind a
  persisted-and-empty snapshot that a later `cleanup` would read as evidence that nothing is
  installed or warmed anywhere. That save is not best-effort: a failed save fails the preview
  exactly as it fails a real run, and a would-fail verdict or `lock --frozen` drift keeps its own
  code with the save error appended (see [Exit codes](exit-codes.md)).
  For `install` and `warm`, each collection is reported as would install/would warm, already up to
  date/already warm, or would fail; each role the same, with the kind named - `Would install (role)`,
  `Up to date (role)`, `Would warm (role)`, `Already warm (role)` - followed by a roles summary line
  printed only when the run has roles. A dry run still fetches a role's or a git collection's
  repository, and downloads a url source's tarball, whenever the source has no usable recorded pin,
  since only the fetched content tells its identity and dependencies; it discards what it fetched,
  commits nothing to the cache, and never creates the roles directory. A role whose directory
  exists and was installed neither by this tool nor by `ansible-galaxy` is a would-fail with the install-failure code (`5`), the same refusal
  a real install makes before fetching anything. A would-fail verdict covers, for both commands, the artifact
  not being cached while `--offline` forbids downloading it (exits with the install-failure code,
  `5`), or the cached artifact's own recorded digest - itself well-formed - disagreeing with the
  lockfile pin while `--offline` forbids refetching a replacement (exits with the dedicated
  integrity code, `7`). The well-formedness gate sits on the recorded digest rather than on the
  pin, which is the opposite of what the phrasing suggests: a malformed *recorded* digest is
  deliberately never a would-fail verdict, while a malformed *pin* still produces one.
  `install` alone adds a third cause, since only it writes an install tree: the collection's
  install directory sits under a namespace path a real install would refuse to write to - an
  escaping symlink, or a regular file blocking it - and extraction would fail the identical way
  (exits with `5`). A dangling namespace symlink is deliberately not a would-fail: a real install
  removes it and creates the directory fresh, so the preview reports the collection normally. Each
  cause exits with the code a real run hitting that same cause would. One asymmetry is deliberate,
  and it depends on the cache backend: the recorded-digest cause can fire where the real run still
  succeeds, because under a pin a real install re-hashes the tarball rather than trusting the
  recorded digest - so on the local cache backend, a cache whose recorded digest was altered while
  its bytes were left intact is refused by the preview and installed for real anyway. On the S3
  backend that same cache fails the real run too, because it re-checks the recorded digest against
  the freshly downloaded bytes before the pin is ever re-hashed - so there the preview's refusal
  matches what actually happens. The preview refuses it either way, because that cache is damaged
  either way. The dry-run banner is printed to stderr and survives `--quiet`, because the flag is
  env-sourced (`$GO_GALAXY_DRY_RUN`) and an org-wide CI environment block would otherwise turn
  every install, warm, or lock run into a silent no-op.
  Before any collection is even resolved, `install --dry-run` also checks whether
  `ansible_collections` itself is usable: a real directory, an in-root relative symlink to one, or
  an absent entry are all fine; an escaping or dangling symlink is refused with the install-failure
  code (`5`), and a regular file sitting there is refused unclassified (exit `1`) - matching a
  real, non-dry-run install exit-for-exit on every one of those shapes, and aborting the whole
  preview before resolution ever starts rather than reporting on any collection at all.
  For `install` and `warm`, a dry run still reports a cached artifact's presence, not its actual
  on-disk bytes: it does compare the artifact cache's own recorded digest against the lockfile pin
  (the integrity would-fail cause above), but a tarball whose bytes silently drift while that
  recorded digest is never updated cannot be detected without re-hashing it - a full object
  download on the S3 backend, the exact cost this preview exists to avoid. Under `--frozen
  --offline`, a collection in exactly that state is still reported cached - `Already warm`,
  `Would warm (artifact cached)`, or `Would install (artifact cached)` - even though the real run
  would fail closed with a checksum-mismatch error. `Up to date` is not affected, because a real
  install skips such a collection without ever opening its tarball. The run prints a one-time
  stderr warning whenever both flags are set together, naming this narrower residual; it is a
  disclosure, not a fix.
  A cached artifact alone does not make `warm` report `Already warm`: the artifact cache (possibly
  a shared S3 bucket) and the local extracted store are independent, so the extracted tree must
  also be ready under the sha256 the collection is pinned to (by the lockfile, or by a url
  source), or, without a pin, the one its warmed entry from the last 30 days records. A fresh runner over a warm bucket therefore reports
  `Would warm (artifact cached)`, and so does a collection with neither a pin nor a warmed entry,
  even when an `install` already extracted its tree - the preview never fetches an artifact just
  to learn its sha256.
  For `lock`, a dry run builds the lockfile in memory from a fresh resolve, loads whatever
  lockfile is already on disk, and reports how the two differ instead of writing anything. A
  `Would change: server <from> -> <to>` line prints first when the file-level `server` field
  itself would change, followed by one line per added, updated, or removed collection - `Would
  add: <name>@<version>`, `Would update: <name> (<field> <from> -> <to>; ...)`, `Would remove:
  <name>@<version>` - and a trailing summary whose verdict is `lockfile would change` unless
  nothing at all would change, in which case it reads `lockfile is up to date`; a change to only
  the file-level `server` field flips this verdict even though every per-collection count stays
  zero, since that alone would still rewrite the file on a real run. A lockfile already on disk
  that cannot be loaded (a bad schema version, unparseable YAML, or any other read failure) is
  warned about on stderr and then treated the same as no lockfile at all - every collection
  reports as added - because a real `lock` run never reads that file, it only overwrites it, so
  the preview cannot fail on it either.
  **Breaking change (after v1.0.2):** through v1.0.2, `--dry-run` (and `$GO_GALAXY_DRY_RUN`) had
  no effect on `install` or `warm` - only `cleanup` implemented it - so `install --dry-run` performed a
  full, real install and `warm --dry-run` performed a full, real warm; `lock --dry-run` refused to
  run at all, exiting with the usage code (`2`). `install` and `warm` now install and warm
  nothing, and preview instead; `lock` now previews instead of refusing. A CI job that carried an
  ambient `$GO_GALAXY_DRY_RUN` and was really installing collections will now finish successfully
  with nothing installed; a bake job carrying the same ambient variable will now finish
  successfully with an empty cache, producing a green build and an empty image; a lock job
  carrying it will now finish successfully having left the lockfile untouched instead of exiting
  with a usage error. In every case the stderr banner above is the only signal, so a job that
  branches on the exit code alone will not notice. If a shared `$GO_GALAXY_DRY_RUN` CI environment
  variable is set, scope it to the jobs that actually want it, or unset it for `install`, `warm`,
  and `lock` jobs where it must not silently do nothing. `cleanup` implements its own `--dry-run`
  (see [cleanup options](#cleanup-options)). `hash`, `tree` and `explain` ignore the flag, since
  they have no product and write nothing for it to suppress. `outdated` writes no product either,
  but it does write one externally consumed report - the metrics file - so `--dry-run` suppresses
  that report and prints a stderr warning naming the path, and changes nothing else about the run.

### cleanup options

- `--verbose` - verbose output (`$GO_GALAXY_VERBOSE`)
- `--quiet, -q` (`$GO_GALAXY_QUIET`) - suppress progress and log lines; results, warnings and
  errors still print. Ignored when `--verbose` is also set.
- `--dry-run` (`$GO_GALAXY_DRY_RUN`) - report what `cleanup` would remove and sweep, without
  deleting anything and without saving the snapshot: collections
  (`Would remove <namespace>.<name>@<version>`), roles (`Would remove role <name>`),
  extracted-store entries (`Would sweep extracted "<name>"`) and legacy flat-key artifacts that are
  still cached (`Would sweep legacy artifact <key>`). The summary line prints a candidate count
  instead of a removed count.
- `--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`)
- `--s3-bucket` (`$GO_GALAXY_S3_BUCKET`)
- `--s3-region` (`$GO_GALAXY_S3_REGION`)
- `--s3-prefix` (`$GO_GALAXY_S3_PREFIX`)
- `--s3-access-key` (`$GO_GALAXY_S3_ACCESS_KEY`, `$AWS_ACCESS_KEY_ID`)
- `--s3-secret-key` (`$GO_GALAXY_S3_SECRET_KEY`, `$AWS_SECRET_ACCESS_KEY`)
- `--s3-endpoint` (`$GO_GALAXY_S3_ENDPOINT`)
- `--s3-session-token` (`$GO_GALAXY_S3_SESSION_TOKEN`, `$AWS_SESSION_TOKEN`)
- `--s3-path-style-disabled` (`$GO_GALAXY_S3_PATH_STYLE_DISABLED`) - switch to virtual-hosted-style
  addressing (`<bucket>.<endpoint>/<key>`). Path style (`<endpoint>/<bucket>/<key>`) is the default,
  which is what the flag disables.

`cleanup` aborts with a non-zero exit and deletes nothing if a recorded project's `requirements.yml` fails to load for any reason other than the file no longer existing at all, with one exception: a file whose `collections:` list reads and whose `roles:` list this tool refuses (an `include:`, a local-path `src:`, any other entry ansible accepts and this tool does not) is read for its collections, reported with a warning naming the project, and every role installed under that project's recorded `roles_path` is kept this run, together with every role those depend on wherever it is installed, since an unknown set of role roots could have been protecting any of them - so one such file never poisons every cleanup against a shared cache. A recorded requirements file that no longer exists at all is treated differently: it is a tolerated stale registry entry, reported with a single warning naming the project and contributing no reachability roots this run, rather than a load failure. A project's collections path candidates are tried in order - the recorded collections path, then `<project>/.collections` and `<project>/collections` - and the first one that opens and holds an `ansible_collections` directory is scanned; a candidate that does not open, or whose `ansible_collections` is absent or not a directory (a regular file, a dangling symlink), is passed over silently for the next, and a project with no such candidate is simply not scanned. Only a probe that fails with anything but not-exist - most commonly an `ansible_collections` symlink escaping its collections path, or a symlink loop - skips the project for scanning instead, with its own warning naming the project: nothing under it is scanned or removed, and every other project's cleanup still proceeds unless some recorded project's `requirements.yml` fails to load for any reason other than the file no longer existing at all, which aborts the whole run for every project at once. A skipped project's `requirements.yml` is still resolved against every other recorded project's installed collections, though, so its roots can keep another project's on-disk copy alive even though nothing under the skipped project itself was scanned or removed this run; `--dry-run` still never previews a removal for the skipped project's own collections, since a real run could not perform one there either. Within a project that does get scanned, an individual collection whose `MANIFEST.json` is not a regular file (a symlink, a directory, or anything else in its place), does not parse, or names an unsafe namespace, name or version is skipped with its own warning naming the path and is neither a root nor a removal candidate, while the rest of that project's collections are still scanned and cleaned up normally; a directory with no `MANIFEST.json` at all is not a collection and is skipped silently. Any other I/O error reading a collections tree, such as a directory it may not read, aborts the run before anything is deleted, and a failure removing an unreachable install aborts it too rather than being reported as removed - see [the cleanup flow](commands.md#scan-every-recorded-projects-installs).

A git `collections:` requirement keeps alive the installed collections its recorded pin names. With no pin - a `--clear-cache` run forgot it, or a schema bump dropped the snapshot - it keeps every installed collection recorded from the same repository at its subdir or an immediate child, whatever the commit, since a destructive pass errs toward keeping; a url requirement falls back the same way to every install recorded from its URL, whatever the sha256. Either way, a git requirement that names its collection keeps only that one. A copy recorded from another repository is not kept by it, so unless another root reaches it, its tree, its artifact and any Galaxy dependency only it kept alive are removed.

`cleanup`'s extracted-cache sweep keeps a collection warmed within the last 30 days even if no project currently installs it, so a `warm`-only machine does not lose the extracted trees it exists to produce; a warmed entry that goes stale (no warm run for 30 days) is swept like any other unreferenced entry.

Roles are cleaned by a narrower rule than collections, and the evidence it runs on is deliberately limited. `cleanup` scans only the roles path a project's registry record names (every `install` records it; a record written by a binary that predates roles carries none, and such a project's roles are never scanned - the tool never guesses a sibling of the collections path). Under that directory, only a role directory carrying this tool's own `.extract-done.<sha256>` marker counts: a role `ansible-galaxy` installed (`meta/.galaxy_install_info` alone) or somebody wrote by hand is never touched. A role is reachable when some registered project's `roles:` list names it, or a reachable role's recorded dependencies do; every unreachable role directory is removed through an `os.Root` at its roles path, its cached artifact and its snapshot record with it, and the extracted trees of the roles that stay are kept out of the sweep. A role directory that carries the marker but has no snapshot record (a cache that was cleared, or a different cache directory) is still removed when unreachable; it merely has no artifact to purge.

**Upgrade note (after v1.0.2):** the cache snapshot schema was bumped after v1.0.2 - to schema version 6, which added the warmed-set bucket - so upgrading from v1.0.2 or earlier makes the first `install`, `lock`, or `warm` run rebuild its metadata caches cold. If you run `cleanup` before that first run, it finds no persisted snapshot - the old one was dropped by the schema bump - so it skips the extracted-cache sweep entirely and leaves the snapshot untouched; the metadata caches still rebuild cold on the first `install`, `lock`, or `warm`.

### hash / tree / explain options

These three read-only commands take two flags of their own and none of the
install set - they resolve nothing, open no cache backend, and make no request,
so there is nothing for a worker count, a server or a cache flag to act on:

- `--requirements-file, -r`, also spelled `--role-file` (`$GO_GALAXY_REQUIREMENTS_FILE`,
  `$ANSIBLE_GALAXY_REQUIREMENTS_FILE`)
- `--lock-file` (`$GO_GALAXY_LOCK_FILE`) - defaults to `galaxy.lock`
  beside the requirements file

`hash` covers the roles list: the lockfile hash is computed over the canonical
file, roles included, so a repinned role changes the CI cache key exactly as a
repinned collection does. With no lockfile at that path, `hash` falls back to
the SHA256 of the requirements file's bytes exactly as they are on disk. The
file is not parsed or validated, so a malformed requirements file still yields
a key, and unlike the canonical lockfile hash, any edit to it, whitespace and
comments included, changes the key. A requirements file that is missing or
cannot be read exits `2`. A lockfile that exists but fails to load is an error
(`6`), never a fallback.

The four global options (`--verbose`, `--quiet`, `--dry-run`, `--cache-dir`)
are still accepted, since the root command declares them for every subcommand,
but none of the three has a write to suppress or a cache to place.

## Color

Every line carries a status marker: `✔` success, `↑` a newer version exists,
`✗` failure, `!` warning, and a gray `·` for everything that reports what the
run is doing rather than what it concluded. One glyph per kind, one column
wide, so a log reads as a single column of text with a single column of
markers beside it. The markers are colored only when the stream they are
written to is a terminal, decided per stream: with
`go-galaxy install > install.log`, stdout gets plain
text while stderr, still a terminal, keeps its color. Redirecting both leaves
the log free of escape sequences, so `grep '^✗'` matches the lines it names.
`↑` is yellow like `!` and means the same kind of thing - something to look
at - but it is a verdict about one collection on stdout rather than a warning
on stderr, so `outdated`'s three per-entry lines each carry their own marker
and line up under each other.

The version an `Installed:`, `Cached:` or `Failed:` line settled on, and the
one on `outdated`'s `--verbose`-only `Up to date:` line, is the other thing
this rule governs. It is written as `== <version>` after the name,
dimmed on a stream that accepts color and plain text on one that does not, so
`grep '== 1.2.3'` finds it in a redirected log exactly as it reads on a
terminal. On a failure line it sits between the name and the cause, since a
version behind the cause would read as part of the error text.

Two environment variables override that check:

| Variable                        | Effect                                                     |
|---------------------------------|------------------------------------------------------------|
| `NO_COLOR`                      | Set to any non-empty value: never emit color.               |
| `CLICOLOR_FORCE` / `FORCE_COLOR`| Set to any non-empty value other than `0`: always emit color, terminal or not. |

`NO_COLOR` wins when both are set: it is an opt-out, and an opt-out another
variable can override is not one. The force variables exist for a CI that is
not a terminal but does render escape sequences in its log viewer. A value of
`0` for either force variable means "do not force" and falls through to the
terminal check rather than disabling color outright.

The spinner is a separate decision and is not affected by `NO_COLOR`: it is
drawn only when stdout is a terminal, and under `NO_COLOR` it still runs, just
without color. `TERM=dumb` is narrower still: it removes the color from the
spinner's frames while leaving the status markers colored.

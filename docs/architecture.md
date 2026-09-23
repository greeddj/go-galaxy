# How it works

This document describes the mechanism rather than the behavior. What go-galaxy
does is documented in the [CLI reference](cli.md) and the pages beside it; this
is how it does it, for anyone reading or changing the code.

## Layering

The import graph is acyclic and shallow. `cmd/go-galaxy` is wiring,
`internal/galaxy/*` is the work, `internal/cache/*` is persistence behind a
seam, and a few leaf packages hold cross-cutting concerns.

```
cmd/go-galaxy            main, signal handling, exit-code decision
  cliflags               the flag sets and their defaults
  commands               the urfave/cli command tree
  exitcode               sentinel errors and signals -> exit codes
  buildinfo              version string

internal/galaxy/collections   the resolve-download-verify-extract-record pipeline,
                              for collections and (role_*.go) for roles
internal/galaxy/cleanup       reachability and removal, collections and roles
internal/cache                the only factory choosing a concrete backend
  local                       BoltDB snapshot + JSON registry + flock
  s3                          gzipped-JSON objects, hand-rolled SigV4, distributed lock

internal/galaxy/cache         the Backend / ArtifactStore seam, decorators, cache policy
internal/galaxy/store         the persisted snapshot as an in-memory value
internal/galaxy/solver        the version solver: pure, no I/O
internal/galaxy/archive       tar.gz extraction with the refusal rules
internal/galaxy/extracted     the content-addressed tree store
internal/galaxy/manifest      MANIFEST.json / FILES.json, read-only
internal/galaxy/signature     OpenPGP verification, read-only
internal/galaxy/gitsource     the git grammar (URL, ref, subdir, locator, credentials)
                              and the Client seam; imports no go-git
internal/galaxy/urlsource     the url-source grammar (URL, credential-binding prefix,
                              locator); no transport of its own
internal/galaxy/gitfetch      the only go-git importer: advertise, fetch by hash,
                              read the tree; implements Client for collections and roles
internal/galaxy/tartree       a downloaded role tarball as a treearchive.Source:
                              extract through archive, detect the role root
internal/galaxy/treearchive   the only production tar writer: a tree -> a deterministic tar.gz
internal/galaxy/collectionbuild  what makes a tree a collection: ignore rules, MANIFEST.json,
                              FILES.json, identity from galaxy.yml; drives treearchive
internal/galaxy/rolebuild     what makes a tree a role: meta/main.yml, its dependencies,
                              no lead documents; drives treearchive
internal/galaxy/galaxyv1      the Galaxy v1 role API client and ansible's version selection
internal/galaxy/requirements  requirements.yml parsing, collections and roles
internal/galaxy/config        flags + ansible.cfg + environment -> one Config
internal/galaxy/lockfile      galaxy.lock
internal/galaxy/infra         the per-run DI container
internal/galaxy/fetch         the shared HTTP client and its per-origin policy
internal/galaxy/metrics       the run counters
internal/galaxy/helpers       sentinels, size caps, tuning constants, validation
internal/gzipstream           the only place a gzip reader opens over untrusted bytes
internal/galaxy/output        the Printer interface and its output tiers
internal/progress             the Printer implementation
internal/safeout              control-sequence stripping
```

Two facts about this graph are load-bearing rather than incidental.

**`internal/galaxy/solver` imports only `helpers`.** No config, no store, no
HTTP. That is the purity claim made structural: the solver cannot reach I/O
even by accident, and everything it learns arrives through its `Provider` seam.

**`internal/galaxy/helpers` sits at the bottom**, above only `internal/safeout`.
It owns the sentinel errors that `errors.Is` matches across every layer, the
size caps and tuning constants, and the validation predicates - `IsPathElement`,
`IsCollectionName`, `IsSHA256Hex`, `IsExactVersion`, and for roles `IsRoleName`,
`IsRoleInstallName` and `IsRoleVersion`, which are deliberately not derived
from the collection alphabet (real role names carry hyphens, upper-case
letters and leading digits) nor from `IsPathElement` alone. A value's shape is
judged by one rule at the boundary it enters through, never re-derived
downstream.

A collection name has two alphabets, picked by source kind, and neither is a
path-safety rule. `IsCollectionNamePart` holds a server-resolved namespace or
name to `^[a-z][a-z0-9_]*$`, what galaxy.ansible.com and Automation Hub
accept; `IsURLCollectionNamePart` holds a url collection's `MANIFEST.json`
identity to ansible's FQCN word rule, `[A-Za-z0-9_]+`, since that manifest is
written outside any Galaxy server and real release artifacts carry mixed-case
namespaces. Every reader of a collection identity - the lockfile,
`collectionbuild`'s manifest parsing, url discovery and `outdated`'s
installed-tree reader - picks the alphabet by entry type. `SplitFQDN` checks
shape only (`foo/bar.baz` splits), so a caller joining the halves into a path
applies `IsPathElement` itself. `IsExactVersion`, by contrast, is relied on as
a path check: whatever semver accepts as one version, under either grammar its
mutable global `CoerceNewVersion` selects, is also a path element, so a caller
building a path from a version checks only `IsExactVersion`. An exhaustive
test and a fuzz target pin that implication, since a semver upgrade that
widened either grammar would reopen a path traversal.

### What each package owns

`internal/galaxy/infra` is the per-run container: the printer, the shared
Galaxy HTTP client, the url-source client (`URLHTTP`), the git client (`Git`,
an interface, wired from `gitfetch` in production and from a double in tests)
with its revealed credentials, the clock, the temp-dir accessor, the metrics
counters, and five test-only deadline overrides - artifact download, metadata
fetch, state object, signature fetch and git fetch - that are reachable through
accessors so a nil container or a non-positive override falls back
structurally. The git budget falls back to the artifact download deadline,
since one git acquisition, advertisement to last built artifact, is an
artifact download. Nothing wires those overrides to a flag, an environment
variable or an ansible.cfg key. A nil `URLHTTP` or `Git` met at acquisition
time is reported as a wiring error and never replaced by the shared client,
which would attach Galaxy tokens and relaxed TLS to repository-authored URLs.
Its `--verbose` configuration lines render a Galaxy token only as a presence
boolean, and a git or url credential's secret not even that. Extend `Infra`
rather than adding a global or widening a signature.

`cmd/go-galaxy` is wiring only. `newRootCommand` builds the command tree with
`install` as the default command and `commands.NoArguments` as the root's
argument validator, which every command inherits: a command added later
refuses positional arguments until it declares a validator of its own, as
`explain` does. `run` installs the SIGINT, SIGTERM and SIGHUP handler whose
cancellation every command runs under - SIGQUIT is left to Go's goroutine
dump - and `handleResult` turns the outcome into an exit code (see
[Exit codes](#exit-codes)). `Version`, `Commit`, `Date` and `BuiltBy` in
package `main` are the binary's only build-info source, set by `-X` ldflags in
both the Justfile and `.goreleaser.yml`; `-X` silently does nothing for a name
that does not exist, so renaming them or moving them out of `main` leaves a
release reporting only what `buildinfo` can recover. `buildinfo` renders
`--version` as `<version> (commit <c>, built by <b> @ <date>) // <go version>`,
dropping the commit or date clause when it is empty. The link-time values are
authoritative; a plain `go build` or `go run` fills each empty field from
`runtime/debug.ReadBuildInfo` (the main module version, `vcs.revision`,
`vcs.time`) and never from the network.

`cmd/go-galaxy/cliflags` owns the flag surface only: names, aliases, the
defaults `--help` advertises, and the environment variables urfave reads as
each flag's sources. It reads no value back; turning a parsed command into
configuration belongs to `internal/galaxy/config`. The signature flags are a
separate set mounted only by `install` and `warm`, the commands that verify, so
`lock` and `outdated` do not advertise settings they would ignore.
`cmd/go-galaxy/commands` is the command tree and nothing else: each command
declares its flags and hands the parsed command to an entry point under
`internal/galaxy`. `install`, `cleanup`, `lock`, `warm` and `outdated` share
`runCollectionCommand`, which builds the `Config`, the printer and the
`Infra`, and wires the git client and the url client for every one of them
alike - neither holds a connection until a requirement needs one. `hash`,
`tree` and `explain` build no config, cache backend or HTTP client.

`internal/galaxy/config` turns the parsed command, the environment and a named
or discovered ansible.cfg into one `Config` per run; see
[Configuration](#configuration). `internal/galaxy/fetch` builds every
`*http.Client` the program uses and owns their per-origin credential and TLS
policy; see [The HTTP clients](#the-http-clients). It never imports `config`,
the layer above it. `internal/galaxy/output` declares only the `Printer`
interface and imports nothing but `time`, so any package, and any test through
a recorder, can take a printer without pulling in the renderer;
`internal/progress` is its one implementation and `internal/safeout` the
sanitizer every printed payload passes through, both described in
[Operator output](#operator-output).

`internal/gzipstream` walks a stream member by member itself, with pgzip's own
multistream loop switched off, and holds three invariants a refactor can break
silently. Every terminal return - a hard error, the empty-member refusal, the
`io.EOF` a failed `Reset` reports at the end of the stream - is stored and
repeated by every later read without calling pgzip again, because pgzip blocks
on a read after a member's end with no context to break it; on the S3 path,
read under the distributed lock, that would leave the heartbeat renewing a lock
the run never releases. The reader handed to every `Reset` must be the very
`*bufio.Reader` the decompressor was built on, since pgzip wraps any other
source in a fresh buffer and drops what the previous one read past a member's
boundary, truncating a multi-member stream. And the context check sits under
that buffer, on the compressed side, since a member of zero-length stored
blocks consumes wire bytes without producing any and the decompressed-side
checks in `archive` and `manifest` never fire on it; it returns `ctx.Err()`
unwrapped, so the exit code still classifies a cancellation or a deadline.

`internal/galaxy/requirements` is where requirements.yml, which is repository
content, enters the program, and every value in it is judged there rather than
downstream. The collection name alphabet is applied once an entry's string and
mapping shapes converge, not inside `helpers.SplitFQDN`, which a mapping's
explicit `namespace:`/`name:` pair never passes through. A `signatures:` source
is judged by the grammar the fetch itself uses (see
[Verify, extract, record](#verify-extract-record)), git and url entries by the
`gitsource` and `urlsource` grammars, which refuse a credential before any
message renders the URL, and a role entry - like every dependency a fetched
role's meta declares, through `ParseRoleDependency` - by the role alphabets and
those same grammars. An unnamed git entry and every url entry have no identity
at load, so their name is judged at discovery instead: `IsCollectionNamePart`
over the repository's `galaxy.yml`, `IsURLCollectionNamePart` over the
artifact's `MANIFEST.json`.

`internal/galaxy/gitfetch` drives go-git at the upload-pack session level
rather than through its `Remote`, and the advertisement decides everything: the
commit a ref names, whether a deepen may be sent (only when the remote
advertised `shallow`, since a deepen to one that did not is a protocol error)
and whether a commit may be wanted by hash. A want is always a hash, never a
name, so a ref moved between advertisement and fetch cannot substitute content;
an annotated tag is wanted as its tag object,
since upload-pack answers a want for a peeled commit that is not itself a tip
with "not our ref". The package reads the advertisement's raw fields
(references, peeled map, `HEAD`, the `symref` capability) and never resolves
through go-git's `AdvRefs.AllReferences` or the stock clone and fetch built on
it, which, with no `symref`, guess `HEAD`'s branch by hash and fail outright
against a remote whose `HEAD` is detached on a commit no branch points at. The
only second advertisement is the fetch-by-search fallback, on a fresh session,
because an ssh session is one upload-pack command whose channel closes with the
first pack. The http(s) transport is built per fetcher from the run's git
client (`fetch.NewGit`), never taken from go-git's process-global registry, so
that client is the only one a git credential travels over. `gitfetch.New` also
hardens the process once: it deregisters go-git's `file` transport (which execs
`git-upload-pack`) and `git` transport (plaintext TCP) behind the URL grammar's
own scheme refusal, and nils `DefaultSSHConfig`, so a `~/.ssh/config`
`Hostname` or `Port` cannot redirect which host a run dials and verifies.

`internal/galaxy/treearchive` is the only production tar writer:
`collectionbuild` and `rolebuild` decide what a tree means and hand it the walk
and the write, so collection and role artifacts share one byte shape, one
budget and one symlink policy. It reads a tree only through its `Source`
interface (a commit tree from `gitfetch`, an extracted role tarball from
`tartree`, `faketree` in tests) and never the filesystem; a `Source` must
validate every name `ReadDir` returns, failing on an invalid one rather than
skipping it, and cap `Open` at the per-entry size. A build is two passes
because a collection's lead documents describe the tree behind them: `PlanTree`
walks parents before children, applies the ignore rules and charges every cap
as it goes (entry count with the lead documents reserved, per-entry and total
declared size, name and link-target length, depth), so an oversized tree is
refused before a byte is written; `Plan.Write` then streams the lead documents
and the planned entries, refusing a blob whose streamed length differs from
what its tree entry declared (`ErrGitCommitMismatch`). Every entry carries the
commit time truncated to the second, a fixed mode (`0644`, `0755` for an
executable or a directory, `0777` for a symlink) and no owner, and the gzip
header is left unset, so two builds of one commit by one Go toolchain are
byte-identical. A kept symlink is rewritten to point straight at the final
entry its chain resolves to, relative to its own directory, because the
manifest chain reader resolves one lookup per hop over real entries and the
extractor refuses a path through a link; the writer's hop limit, `linkMaxHops`,
must equal the reader's `chainMaxLinkHops`, or an artifact could carry a chain
the reader refuses.

`internal/galaxy/collectionbuild` adds what makes the result a collection,
reading the tree only through `treearchive.Source` and writing only into the
temp file its caller supplies: ansible's default ignore list with
`build_ignore` appended (fnmatch against the root-relative path, plus a
basename prune list applied at every depth); the lead documents,
`MANIFEST.json` then `FILES.json`, the first binding the second by the sha256
of its exact bytes, both encoded as ansible's Python encoder writes them (a
one-space indent, no HTML escaping); and the identity, read from `galaxy.yml`,
or from `MANIFEST.json`'s `collection_info` for a tree that ships a built
collection. `ParseManifestInfo`, the url source's identity reader, lives here
too. A `FILES.json` over its cap is measured and refused before a byte is
written. Every artifact is read back through the manifest chain reader before
`Build` returns it, and a failure there is a builder defect,
`ErrGitArtifactSelfCheck`, carrying its cause as text rather than wrapping it,
so it can never classify as an integrity failure of the remote's bytes.

`internal/galaxy/rolebuild` is its role counterpart. It carries each dependency
spec from `meta/main.yml` and `meta/requirements.yml` exactly as written - a
string is not split at its commas, an `scm+url` source not at its plus -
because `requirements.ParseRoleDependency` is the one place that grammar is
judged, and drops silently the keys ansible passes on as role parameters
(`when:`, role variables). The requirements file's list is appended behind the
meta's dependencies, as `ansible-galaxy` appends it, and that order is what the
first-wins install-name walk sees. `galaxy_info.role_name` is informational
only, and a malformed `galaxy_info` a warning, since ansible never reads it at
install time; the install name belongs to the caller. A `.git` directory is
excluded at any depth, which the git tree reader already refuses but a url
role's tarball could carry. Every artifact is probed for tar.gz shape before it
is handed over, a failure reported under `ErrGitArtifactSelfCheck` the same
way, while the caller's cancellation passes through unchanged.

`internal/galaxy/tartree` is the one place a foreign role tarball becomes a
`treearchive.Source`, and it never parses the bytes itself: `Load` hands them
to `archive.ExtractTarGz`, which owns every cap and refusal, extracting into a
private `go-galaxy-role-tar-*` directory under the run's temp directory that is
removed again on any error. The role root is ansible's shortest parent of
`meta/main.yml`, held to depth two: the archive root if it carries
`meta/main.yml` or `meta/main.yaml` as a regular file (a symlinked meta does not
count, as in `rolebuild`), else the single top-level directory that does; none
or several is `ErrRoleTarballLayout`. The tree serves the extraction through an
`os.Root`, lists entries in byte order - the only deterministic order left once
extraction has flattened the tar's own onto disk - returns a symlink's target
as its blob rather than following it, and refuses a name carrying a control
rune or a backslash (`ErrRoleTarballEntryInvalid`), which the extractor would
refuse when the repacked artifact is installed. Its commit time is always the
Unix epoch, so one origin tarball repacks to one byte sequence under one
toolchain whatever mtimes extraction left on disk.

`internal/galaxy/cache` owns the seam every backend under `internal/cache`
implements (see [The cache seam](#the-cache-seam)) and the behavior that
belongs to the seam rather than to either side: the `Backend` decorators
`WithStateDeadline` and `WithCleanSaveSkip`, the `LockLostError` verdict, and
`FetchJSONWithCachePolicy`, whose `Policy` decides whether a metadata request
may read or write the snapshot's API response cache. It sits on the
business-logic side of the seam, so a unit of work's budget is set by the code
that owns the unit and never inside a concrete backend: the metadata budget in
`fetchJSONBody`, below the collections wrapper so no direct caller escapes it,
the state-object budget in `WithStateDeadline`, the artifact download budget in
`internal/galaxy/collections`. Both decorators implement every `Backend` method
by hand over a named field instead of embedding the interface, so a method
added to `Backend` fails the build at each constructor until someone decides
whether it needs an override, and each returns nil for a nil `Backend` rather
than a wrapper around nil. A new backend is bound by the interface contracts,
not by what today's two happen to do.

`internal/galaxy/store` owns the snapshot as one in-memory value, `Store`, and
the local plumbing around it: the Bolt file (`OpenDBs`, `Load`, `Save`), the
project registry, the instance lock (`AcquireLock`) and the cache-directory
sweeps (`ClearCacheFiles`, `SweepDownloadTemps`). `Store` guards its maps with
its own `sync.RWMutex` and shares no mutable state with its callers, since a
caller mutating a shared slice or map would bypass that lock and change what is
later persisted: every setter clones a reference-typed field before storing it
(`SetRequirements` deep-copies each spec's `Signatures`, which copying the spec
map alone would leave on the caller's array), every getter returns a copy, and
a new accessor over such a field must do the same. The one exception is
`GetAPICache`, whose `Body` shares the stored array on the warm-cache hot path,
so every caller treats it as read-only. `snapshotData` builds the save payload
under the read lock and the payload is marshaled after it is released, with an
API body and an uncut `Signatures` slice passed through uncloned; that is safe
only because every mutator replaces rather than edits, so one writing into an
existing array in place would race a concurrent save. Both backends persist
through `snapshotData` (the deep copy that applies age eviction and the
signature-query cut) and `stampSaveMeta` (the schema version and the two save
stamps) - the local backend bucket-maps the result into Bolt, the S3 backend
marshals it to JSON (`MarshalSnapshot`) - so the two cannot drift on what a
snapshot holds or claims about itself. `ValidateSchema` accepts only the
current schema version: an older one loads as a fresh empty store, a newer one
is refused.

`internal/galaxy/extracted` owns `<cache>/extracted` (see [Extracted
store](#extracted-store-content-addressed-materialized-by-hardlink)). A sha
becomes a path only through `entryRel` (`helpers.IsPathElement`), checked
before any `os.Root` is opened, because a sha arrives unvalidated from a
lockfile pin or a snapshot record: an unsafe one is refused (`ErrSHAUnsafe`
from `Ensure` and `Promote`, false from `Ready`, a no-op from `Remove`) without
even creating the cache directory. A store directory that is a symlink leading
out of the cache directory, or not a directory at all, is reported as
`ErrStoreDirUnusable`, since the root itself answers only a bare "file exists".
Two steps deliberately take plain paths: the untar into a temp directory the
root created moments earlier, where nothing can have been planted and
`archive` checks each entry's symlink parents, and `Materialize`, which walks
the ready tree without following symlinks and hardlinks or copies it into the
install directory; a local writer racing between `Ensure`'s return and that
walk is an accepted residual, the same one the install side carries for its
extraction target. The temp entries, `ingest-<random>` and `<sha>.tmp`, are
removed by `SweepTemp` only under the exclusive cache lock, since otherwise a
live run's in-flight temps would match.

`internal/galaxy/lockfile` owns reading, validating and writing `galaxy.lock`.
`Load` is its trust boundary, since a committed lockfile is repository
content, and `Compare` is the drift contract `lock --frozen` gates on; both are
described under [The lockfile](#the-lockfile).

## Configuration

`config.BuildCollectionConfig` is the only place precedence between flags, the
environment and ansible.cfg is decided, `discoverAnsibleConfigPath` the only
place an ansible.cfg candidate is accepted or refused, and `resolveServers` the
only place the server list and each server's credential and TLS policy are
settled. Every credential the package produces is a `Secret`, redacted on every
serialization path and readable only through `Reveal`. It makes no network
request, opens no cache backend and runs before a printer exists, so it prints
nothing: a non-fatal problem is queued on `Config.Warnings`, which every
command prints through `Infra.WarnConfig`, or - for anything about
`roles_path` - on `Config.RoleWarnings`, printed only once a requirements file
with a `roles:` block has been read. `Config.Server` is always
`Config.Servers[0].URL` and exists for single-server consumers (metrics,
`GALAXY.yml`, the lockfile's `server`); anything that dispatches credentials or
TLS per origin reads `Config.Servers`.

`BuildCollectionConfig` reads the union of every flag a command on the
`runCollectionCommand` path may register, and urfave returns the zero value
for a flag the command did not register rather than failing. A command need
not register them all - `cleanup` registers only the S3 flags beside the four
global ones, and reads only the cache directory, `--dry-run` and the S3
settings - but it must register every flag whose `Config` field it reads, or it
silently sees a zero value. Each zero value is defused where it is consumed: an
empty `--timeout` becomes the default no-progress budget, never an unbounded
client; a zero worker count takes the derived default without a warning;
`[galaxy] server_timeout` is not parsed at all for a command without
`--timeout`; and an empty implicit server URL, which is what a command without
`--server` reads when no `[galaxy] server` is configured, yields an empty
server rather than an error, so `cleanup` never fails over a setting it does
not read.

The signature surface is resolved and validated for every command on this
path, the non-verifying ones included, which is why an unset required count
falls back to the default instead of handing the policy parser an empty spec.
Validation builds a signature policy and discards it, only to move a malformed
count or status code to configuration time (exit `2`) rather than to an install
worker after downloads have started; the consumer builds its own from the same
values and still handles the error. The warning that a discovered ansible.cfg
carries signature keys is returned to the verifying commands rather than
queued on `Config.Warnings`, which every command prints.

The order of the steps fixes which error a configuration broken in several
places reports first: `--timeout`, loading ansible.cfg, the server list, git
credentials, url credentials, the S3 cache, the signature surface, then
`[galaxy] server_timeout`. A new check goes after the existing ones, so their
failures keep their precedence.

## The version solver

`internal/galaxy/solver` is a PubGrub-style solver: it chooses one version per
package satisfying every declared constraint, or proves no such choice exists.
It is pure, in-memory and deterministic, and performs no I/O of its own.

### Units

- **Term** - "package in set" (positive) or "not (package in set)" (negative).
  Negating a term flips its sign; the set is never complemented in place.
- **Incompatibility** - a set of terms that cannot all hold. Normalized to at
  most one term per package, sorted by package name, with tautological terms
  and redundant positive root terms dropped - but only while more than one term
  remains, so a genuinely single-term incompatibility is never emptied.
- **Assignment** - a term recorded at a decision level, either a *decision* (a
  version chosen for a package) or a *derivation* (a term forced by an
  incompatibility, which records which one).
- **Partial solution** - the assignment list plus, per package, the signed
  conjunction of every term assigned to it so far. That accumulation is
  maintained incrementally from the first assignment on, and it is exact
  whether or not the package's published version list was ever fetched.

Signed conjunction follows four rules and nothing else: `P(a)&P(b) = P(a&b)`,
`P(a)&N(b) = P(a-b)`, `N(a)&P(b) = P(b-a)`, `N(a)&N(b) = N(a|b)`. Entailment is
the matching four cases, with one asymmetry worth knowing: a negative term never
entails a positive one.

Dropping a positive root term is sound only because root is decided first and
unconditionally, so its term always holds. It must never be done for another
package, whose positive term asserts that something requires it at all. No
stored incompatibility holds a tautological term - the only one is `N({})`,
since a positive term never is - and conflict resolution relies on that: the
empty prefix satisfies no stored term, so every satisfied incompatibility has a
satisfier, and a missing one is reported as a solver bug. For the same reason a
dependency whose constraint denotes the empty set is stored as the single term
`{parent@version}`, not as `{parent@version, not dep in {}}`.

### Version sets

A version set is two independent sublines covering the whole version space,
quotiented by semver precedence so build metadata is invisible:

- the **release subline**, ordered by `(major, minor, patch)`, which has a total
  successor and so decomposes into half-open runs;
- the **prerelease subline**, ordered by full semver precedence, with
  metadata-free bounds.

The factoring is forced rather than chosen. The constraint grammar this project
speaks gates prereleases at the level of an AND group, which makes a constraint
set non-convex over a single order: "every release at or above 1.2.0" has no
interval form there, but is one run per subline here.

Each subline is kept canonical - sorted, nonempty, disjoint, non-abutting - so
structural equality *is* set equality, and the canonical byte encoding an
incompatibility is hashed by is injective. Intersection and complement preserve
canonicality directly; union is derived from them.

Two fields ride along beside the pieces. A singleton's original registry
spelling, which set equality deliberately ignores so it never affects set
identity, but which the exact-pin fast path and decision extraction do read. And
a display label for proof output, which is the one field never branched on for
any logic decision.

### Where prereleases are admitted

`semver.NewConstraint` stays the sole authority on what parses; only after it
accepts does an in-file mirror parser build the set. The mirror replicates the
vendored release's branch structure exactly, and a mirror failure on input the
authority accepted is reported as a solver bug rather than guessed around.

The gate itself is one rule: **an AND group in which no comparator's comparison
version carries a prerelease matches no prerelease version at all**, whatever
the individual runs would admit. A prerelease operand anywhere in the group -
including an exact pin on a prerelease - opens it for the whole group. That is
the mechanism behind both halves of the user-visible rule in
[Compatibility with ansible-galaxy](ansible-galaxy-compat.md#deliberate-differences):
stricter, in that a collection publishing only prereleases satisfies no plain
constraint; looser, in that `>=1.0.0-0` also admits `2.0.0-rc1`.

One shape is refused rather than approximated: a `!=` carrying both an x-range
patch and a prerelease operand excludes an infinite comb rather than a finite
union of runs, and admitting a comb piece kind would destroy closure under
complement.

### The loop

```
add the root incompatibility
loop:
    bail out if the iteration budget is exhausted (a solver defect, not a large input)
    check for cancellation
    unit-propagate from the package that just changed
    make a decision; if there is nothing left to decide, extract the result
```

**Propagation** walks each package's incompatibilities newest-first, since
conflict resolution tends to produce more general ones later. An incompatibility
with exactly one inconclusive term derives that term's negation; one that is
fully satisfied is a conflict and goes to resolution. Relating a term consults
only the partial solution's own accumulation - never a published version list -
so propagation and conflict resolution do zero I/O.

**Conflict resolution** finds the earliest assignment whose prefix satisfies the
conflicting incompatibility, and either backjumps (when that satisfier is a
decision, or when the previous satisfier sits at a different level) or merges
the incompatibility with the satisfier's own cause and repeats. The merged
incompatibility records both parents, which is what makes the derivation graph
reconstructible for the failure proof.

**Backtracking** truncates the assignment list and rebuilds every package's
bookkeeping by replaying the survivors. At Galaxy scale that full rebuild costs
microseconds, so no per-level snapshot machinery is kept. A package that loses
every assignment simply drops out: with exact sets there is nothing to preserve,
because two symbolically different constraints denoting the same set *are* the
same set.

**Deciding** picks a package by a frozen priority - exact pins first, then
highest conflict count, then, among packages whose version list is already
fetched, the fewest allowed candidates, then name order - with ties broken by
name ascending at every step. That third tier ranks only fetched packages, so a
pool in which none has been fetched falls straight through to name order.

Two fast paths keep the common case off the paginated version-list fetch
entirely: an exact pin decides without fetching a list at all, and an unfetched
package with a positive accumulation is probed with the registry's own reported
highest version and decided on it if it passes. Neither is free of the provider -
both still ask for the decided version's dependencies, and the probe costs a root
metadata resolve on top - but only when neither applies is the full version list
paged through.

An empty candidate pool - no undecided package with a positive accumulation -
is a finished solve. That is sound only because every requirement of a decided
parent carries a positive accumulation, which negative facts alone never settle
vacuously, and result extraction backs it up by failing closed: an undecided
package reachable from root through decided dependency edges is a solver bug.
A version's dependency incompatibilities are added before its decision is
recorded, and when one of them would already be satisfied against the
tentative decision, the decision is declined and the package left to the next
propagation round. The dependencies asked for each package and version are
remembered, so a repeated attempt never asks the provider twice.

### The Provider seam

```go
Highest(ctx, pkg)          (Version, bool, error)              // registry-reported highest, unchecked
Universe(ctx, pkg)         ([]Version, error)                  // all published versions
Dependencies(ctx, pkg, v)  (map[string]Constraint, error)      // validated fqdn -> constraint
```

An unknown package is an empty slice and a nil error, not an error. Key and
constraint validation happens inside `Dependencies`: a malformed dependency key
is a provider contract violation the core never guesses around. The production
implementation lives in `internal/galaxy/collections` and is what turns these
three questions into cached Galaxy API calls, recording as it goes which server
answered for each collection - that binding is what the resolved graph's
`source` is taken from. A collection pinned by a `source:` needs no such
evidence: it has exactly one candidate server, so that server is what the
resolved graph records for it whether or not the solve ever had to ask one -
under `--no-deps` an exactly pinned root is settled without asking any.

`--no-deps` is a wrapper whose `Dependencies` always returns an empty map
without consulting the wrapped provider.

A dependency map is cached per server: its `deps_cache` key is scoped to the
base of the server the collection is bound to, since two servers can publish
one `name@version` with different dependencies and an unscoped key would let
whichever answered first decide both. `Dependencies` therefore settles the
bound server before it reads the cache. With a single candidate server (a
`source:` pin, or exactly one configured server) that costs nothing, so a warm
single-server run answers dependencies from the cache alone; with several, it
reuses the binding an earlier call in this solve recorded, and fetches root
metadata only when there is none. Every successful root-metadata fetch records
its binding, whatever the caller then does with the answer, so a package later
decided through `Universe` is still stamped with the server that served it.

`Universe` pages a collection's `versions_url` 100 versions at a time and fails
with `ErrVersionsPagingExceeded` after 100 requests rather than truncating the
list; a declared total implying more than that fails before any further
request or allocation. Page 0 is fetched alone, and its total only schedules
the remaining offsets, which are fetched concurrently on the
`--download-workers` pool. The total is never trusted for termination: pages
are consumed in offset order, the walk stops at the first short or empty page
or once the next offset passes that page's own total, later scheduled results
are discarded and unscheduled pages fetched on demand, so the list equals what
a strictly sequential walk would collect. Every page spends the one metadata
deadline, the only metadata operation that shares a budget across requests,
because the server's count chooses how many pages there are and a per-request
budget alone would not bound the walk.

A solve drives its provider from one goroutine: `Solve` makes every provider
call from the goroutine that called it, and checks its context once per
main-loop iteration, returning a cancellation found there as a bare
`ctx.Err()`, never as a solver bug. Only decision making calls the provider, so
a cancellation that lands during a provider call is the provider's to observe:
an implementation must carry the supplied context into every I/O it performs
and surface a cancellation as an error that still matches `ctx.Err()` through
`errors.Is`, or an interrupt during resolution cannot classify as one. A call
answered from memory need not check it, and a provider shared by concurrent
solves must be safe for concurrent use. The production provider relies on the
single goroutine: its map of which server answered for each collection has no
lock and is read only after `Solve` returns, so the root-metadata prewarm
builds a fresh provider per root. The prewarm's providers and the solve's own
are all built by `newMetadataProviderWithDeps` over one `collectionDeps`,
sharing its mutex-guarded memos and store, so nothing the prewarm fetched is
requested again - building the solve's provider with `NewMetadataProvider`
would silently lose that. Running provider calls in parallel inside `Solve`,
or sharing one provider across workers, needs a lock on that map first.

The prewarm fans the requirement roots out on the `--workers` pool and calls
the very provider methods the solve will call - `Highest` for a root that is
not exactly pinned, `Dependencies` for one that is - so a warmed document is a
cache hit in the solve because both sides made the same call, not because two
implementations agree on URLs, keys and policy. It warms nothing for a git or
url root, nothing for an exact pin under `--no-deps` (the solve asks nothing
for it) and never the version list `Universe` pages. It runs only where the
solve can read back what it writes - a store, at least two roots, and a policy
for the root that both reads and writes - or the run would pay for every
warmed request twice, so `--offline` and `--no-cache` switch it off and
`--refresh` leaves only exactly pinned roots warmed. A 404 is cached nowhere,
so a losing API-root or server probe can still be paid once in the prewarm and
again in the solve. A prewarm error is logged only under `--verbose`, since the
solve reports the classified failure exactly once; it stops further roots being
dispatched but never cancels the solve, which runs on the caller's own context
once every prewarm goroutine has been joined. The call stays below the
snapshot-replay return, because a run that replays the snapshot must make no
metadata request at all.

### Determinism

Every point where Go's map iteration order could leak into the result is
ordered explicitly: the propagation worklist pops the lexicographically smallest
package, candidate packages are sorted, dependency names are sorted,
incompatibility terms are sorted by package, and the published version list is
re-sorted by the core rather than trusted in the order the provider returned it.
That sort is semver precedence descending, tie-broken by the original string
descending, because single-level precedence is not a total order for
equal-precedence spellings such as `1.0.0` and `1.0.0+build` - and "the highest
version" has to be unambiguous.

There are no goroutines, no clock and no I/O in the core.

### How Solve fails

Besides a bare `ctx.Err()`, `Solve` fails in exactly three ways. An
unsatisfiable input is a `*ConflictError`, whose `Is` matches
`helpers.ErrNoVersionSatisfiesConstraints`, the sentinel the exit code
classifies. A provider error is returned wrapped and is never modeled as an
incompatibility. A violated invariant of the solver's own bookkeeping returns
an error wrapping the unexported `errSolverBug` instead of panicking: the main
loop exceeding its budget of 1,000,000 iterations, conflict resolution finding
no satisfier, a decision term without a singleton version, result extraction
reaching an undecided package, the constraint mirror parser refusing what
`semver.NewConstraint` accepted, and the proof walk reaching a non-derived
incompatibility, reported in place of the incomplete proof. A CI pipeline then
gets one red run with a diagnosis, never a crash, a hang or an install of a set
the solver did not prove. The solver's only panic is on the init-time root
version constant; a new invariant check belongs in the
`errSolverBug` family.

Both loops are bounded, the main loop by that budget and conflict resolution by
10,000 steps, and a backjump whose root cause is not almost satisfied - which
signed exact terms rule out by construction - ends as a clean failure rather
than a guess. Backtracking truncates the assignment list before it rebuilds the
per-package bookkeeping, and keeps the old bookkeeping when the rebuild fails,
so a caller that receives that error must abandon the partial solution rather
than continue with it. Result extraction returns only the decisions reachable
from root through decided dependency edges, so a version decided in a branch
the solver later backtracked from is never installed.

### The failure proof

A conflict renders as PubGrub's numbered explanation, built by walking the
derivation graph and emitting a line per node, with line numbers introduced for
nodes referenced more than once, plus one case where a partial-satisfier merge
forces a number early so a later line can refer back to it. Leaves phrase themselves by cause: a
dependency edge, "no version of X matches Y", or "X has no published versions".
When the terminal incompatibility is one that conflict resolution derived, the
final line is rewritten to begin `So,` and end `version solving failed.` A
terminal incompatibility that is external instead - a root requirement whose
constraint denotes the empty set, or a leaf reached through the clean-failure
path - renders as its own one-line description, with neither rewrite applied.

Hints are collected from the no-versions leaves, one per distinct package, and
this is where the prerelease gate becomes visible to an operator: a package
whose every published version is a prerelease is named as such, with an exact
pin or a `>=X.Y.Z-0` floor offered as the remedy. A package that publishes only
some prereleases draws its own, differently worded hint, since there the
excluded versions are a subset rather than everything.

## The install pipeline

`install`, `warm` and `lock` reach the cache backend through one funnel, which
opens the backend, takes its exclusive lock, and runs every piece of work under
the **holder context** that lock returned, judging the outcome against it.
`outdated` deliberately opens no backend at all, and therefore takes no lock and
serves no cached metadata.

That holder-context discipline is the property `internal/lockaudit` gates, over
two closed tables: one naming every function that takes the lock and then does
work under it - the funnel itself and cleanup's own equivalent - and one naming
the three collection commands that must reach the funnel rather than take a
lifecycle of their own. A run whose lock is taken away mid-flight stops rather
than continuing to install and persist without exclusivity. An extraction
observes the context on every read, on both sides of the decompressor (see
[Archive extraction](security.md#archive-extraction)), so it stops mid-entry; a
partial tree is never taken for a finished one, since the extract marker is
written only after the unpack succeeds and the extracted store removes the temp
tree of a failed extraction before anything is promoted. The steps after the
untar take no context - writing the store's ready marker and renaming its tree
into place, hardlinking a finished store tree into the install path
(`extracted.Materialize`), writing the extract marker - so a worker that has
reached them finishes them, but every write to the shared cache ends with the
context.

Both lifecycles, `initInstall` and cleanup's `initCleanup`, return the holder
context on their failure paths after the lock too, not only on success. The
work between taking the lock and returning - the snapshot load, the project
registry load, a `--clear-cache` bulk delete that on a large bucket takes long
enough for a heartbeat to find the lock stolen - can fail because the lock was
lost, and judged without the holder that failure would read as an ordinary
backend or corrupt-snapshot error rather than lock loss (exit `8`). `Open`,
before the lock exists, and `Close`, after ownership ends, take the caller's
own context, and one deferred unwind, registered after a successful `Open`,
releases the lock and closes the backend on every failure path. Cleanup stops
at stated points: before each collection and each role it weighs for removal,
between the removal passes and the sweeps, and before each legacy key and each
extracted-store entry it sweeps; a tree removal already walking and a delete
already issued run to completion (see [the removal
flow](commands.md#removal-unreachable-collections-and-roles)).

### Git discovery

A git requirement has no identity until its repository has been read, so it
is expanded before anything else looks at the roots, as the first step of a
resolve. Per requirement, on the download-worker pool: the recorded pin is
replayed when the cache policy allows a read; otherwise one advertised-
references round trip resolves the ref to a commit and tells whether the
remote allows a shallow fetch or a fetch by hash, the pack for exactly that
commit is streamed into a bare on-disk object store under a byte cap, the
commit's tree is read object by object (never checked out), the collection
directories are located as ansible locates them, and each is built into a
deterministic `tar.gz` with its `MANIFEST.json` and `FILES.json`, self-checked
through the manifest chain reader, and committed to the artifact store under
its locator-scoped key. What discovery learned - identity, exact version,
dependencies - becomes an exact-pin root the solver owns: its universe is that
one version and its dependencies come from its `galaxy.yml`, so no Galaxy
server is ever asked about a git collection, and a Galaxy dependency that
constrains it is satisfied or refused with a proof. The requirements signature
is computed over the expanded roots, so it covers the commit, not just the ref.

An entry that names its collection (`name:` beside a git `source:`) narrows the
repository's collections to that one before anything enters the run's git
memo, and the order is load-bearing: the memo is shared by resolve, prefetch
and install, and the solver answers a git fqdn from it with the git collection
as the single candidate for that name, so an unnamed sibling let in would
silently take its fqdn away from a Galaxy root or dependency of the same name.
The snapshot's pin still records every collection the commit holds, and an
unselected sibling's build is discarded at the narrowing.

### URL discovery

A url requirement is expanded right after the git ones, by the same rules one
dimension simpler: the tarball is downloaded over the dedicated url client
(retried under the artifact deadline, size-capped, probed for tar.gz shape),
its identity and dependency map are read from its own `MANIFEST.json` - the
manifest is the identity document, judged by the same predicates a server's
answer is - and the artifact is committed under its locator-scoped key,
`url+<url>#sha256:<hex>`, the digest of the origin's own bytes. The pin,
keyed by the URL alone, records the sha256, the identity and the
dependencies, which is what a rerun replays without contacting the origin. A
`version:` on the entry is an assertion checked against the manifest on both
the fresh and the replay path. The expanded root is an exact pin exactly as
a git one is, and its sha256 rides the resolution so the install compares
the actual bytes against it on every run.

Only once both expansions are done is the expanded list checked for one fqdn
produced by two roots: a root of either kind has no identity before its own
expansion, so a check inside the git expansion would see every unexpanded url
root under the empty fqdn and report false duplicates. Url roots are
downloaded on the download-worker pool but merged in input order, so the
expanded list, and the requirements signature computed over it, are
deterministic. A pin is replayed whenever the cache policy allows a read, its
TTL ignored - editing the URL is a new key, and only `--refresh` or
`--clear-cache` replaces a pin - and a replayed pin is re-validated through the
same sha256 and identity predicates a fresh manifest passes, since snapshot
state is judged on the way in exactly as an origin's answer is.

### Roles

A role is the other thing a git tree can be, and the pipeline treats it as
such: one more kind of git source, with the Galaxy v1 API as a name service in
front of it. Nothing about a role goes through the solver - a role has no
version constraints, only a ref - so roles are resolved beside the collections
rather than inside them.

**Discovery.** `resolveRoles` walks the `roles:` entries and then, level by
level, the dependencies each fetched role's `meta/main.yml` and
`meta/requirements.yml` declare, breadth-first. Each level is resolved on the
download-worker pool and merged in input order, so which requirement wins an
install name is decided by the file's and the meta's declaration order, never
by which fetch finished first - ansible's first-wins, made deterministic; a
loser with a different source or version is reported. A dependency ansible
would not look up (no dot and no scm: a local role; three parts: a
collection's role) is skipped; one it would refuse is a usage error naming
the role that declared it. `--no-deps` stops the walk at the file's entries;
the walk is capped at 1000 roles. Per role: a git role replays its recorded
pin when the cache policy allows and the artifact is still in the store,
re-advertises under `--refresh` and keeps the pin if the commit is unchanged,
and otherwise fetches the repository through the same session as a collection
(advertise once, fetch by hash into a byte-capped store, read the tree without
a checkout) and hands the tree to `rolebuild`, which finds `meta/main.yml` at
the root, reads the dependencies as written, and drives `treearchive` into a
`tar.gz` with no lead documents and every entry at the top level. A Galaxy
role first asks `galaxyv1` - the record by owner and name, then the version
list, on the configured servers in order, skipping one without a v1 API -
which validates every field, composes the GitHub URL, and selects the tag as
ansible's `GalaxyRole.install` would (LooseVersion highest, the default branch
when there are no tags); from there it is a git role for that repository and
tag, with a second pin keyed by the Galaxy name and the version asked for so a
rerun skips the v1 round trips. What discovery learns is a `resolvedRole`: the
install name, the locator `git+<url>#@<commit>` (the one string its artifact
key, its installed record and its lockfile entry all key on), the repository,
the ref, the concrete version (the tag, the branch, or what `HEAD` resolved
to), the Galaxy name, and the install names of its dependencies. A url role
takes the same road with the download standing where the fetch stands: the
tarball comes over the url client, `tartree` extracts it through the
hardened extractor and finds the role root (the archive root, or the single
top-level directory carrying `meta/main.yml` - anything else is refused),
`rolebuild` repacks it into the canonical artifact, and the pin is the
origin bytes' sha256, keyed `url\n<url>` in the same pin bucket, with the
locator `url+<url>#sha256:<hex>` and a version label defaulting to the
sha's first twelve hex digits.

**Install.** Roles install after the collections, and only when every
collection level went through, on the `--workers` pool and flat - each role is
its own directory, so no order between them is load-bearing. The roles root is
an `os.Root` opened at `roles_path` only when the plan holds a role, so a
collections-only project never grows a roles directory; under `--dry-run` it
is opened without being created. Per role, in order: the skip check (record,
marker and tally agree), then the directory policy - this tool's extract
marker or ansible's `meta/.galaxy_install_info` may be replaced, anything else
is refused before a byte is fetched - then the artifact (a `--no-cache` build
left by discovery, else a cache hit, else a fetch by the pinned commit that
refuses a remote serving another), then extraction through the extracted
store into `<roles_path>/<name>/` with `meta/.galaxy_install_info` written
through the root ahead of the marker so the marker's tally counts it, then the
`installed_roles` record. `meta/.galaxy_install_info` and the marker are each
removed before they are written, never truncated in place: once the tree is
materialized, any file in a role directory can be a read-only hard link into
the shared extracted store, and writing through it would rewrite the store's
bytes under their digest for every install sharing them. The record's
`InstallPath` is absolute, whatever spelling `--roles-path` had, because
cleanup finds a record by the path it scans.

**Lock and frozen.** `lock` resolves roles in the same run and renders each as
a `RoleEntry` pinned by commit, or a url role by its origin bytes' sha256;
under `--frozen` an `install` or `warm` takes
its roles from the lockfile with no network, checking each `roles:` entry as
written against its locked line (same source - the Galaxy name or the
repository - and same ref, and for a Galaxy role with a version asked for, that
version), and `lock --frozen` diffs the fresh role list against the file.
`outdated` asks the remote what a git role's ref points at now, and asks the
v1 API which tag is highest for a Galaxy role, comparing by name.

**Cleanup.** The registry records each project's `roles_path`; `cleanup`
scans only that directory, through an `os.Root` at it, and indexes only a role
directory carrying the extract marker, joined with the `installed_roles`
record for its path when one exists; only a regular file named by the marker
prefix and a sha256-shaped suffix counts as the marker. Reachability is the
project's `roles:` roots plus, transitively, the recorded dependencies of every
installed copy; a project whose `roles:` list is refused takes every role
installed under its roles path as its roots instead, and their dependencies are
followed the same way, wherever another project installed them. An unreachable
role is removed through a fresh root at its roles path, its artifact and
record with it, and the extracted keep set takes the digests of every
installed role that stays. A marked directory with no record (a cleared cache,
another cache directory, a schema bump that dropped the bucket) still takes
part in reachability: its dependencies are read from its own `meta/main.yml`
and `meta/requirements.yml` under the install's dependency grammar, so a role
it depends on is not deleted because the snapshot forgot the edge, and an
unreadable meta contributes none rather than failing the run. Its artifact is
left alone, since the key needs the record.

### Plan construction

Order is load-bearing:

1. Load `requirements.yml`: the `collections:` list and the `roles:` list,
   both judged at load, with the parse warnings (an unknown key on a role
   entry) printed.
2. Build the verification context. This runs **ahead of the prefetcher**, so an
   unreadable keyring, or requirements declaring `signatures:` with none
   configured, fails the run before a single background download is scheduled.
3. Resolve the collections - from the lockfile under `--frozen`, which touches
   no network at all, otherwise through the solver.
4. Resolve the roles (see [Roles](#roles)) - from the lockfile under
   `--frozen`, otherwise through the dependency walk. This too runs ahead of
   the prefetcher, so a role no server knows, or a repository that refuses,
   fails the run before a background download is scheduled.
5. Fold the resolved set into a key-addressed map, rejecting an unsafe
   identifier, a non-exact version, or a duplicate key. This fold is the only
   gate on a version replayed from the snapshot's last resolution, which is
   rebuilt refusing only an empty version, so a poisoned `resolved` bucket
   carrying `*` is stopped here with `ErrInvalidCollectionVersion` before any
   install worker starts; skipping or reordering the fold on that path would
   let a non-version reach install.
6. Check the post-condition that every requirements root came back resolved,
   which is what catches a solve silently dropping one.
7. Compute install levels by topological layering. This happens **before** the
   prefetcher starts, so a dependency cycle surfaces before any prefetch worker
   exists, and the level assignment can order the prefetch queue.
8. Start the prefetcher. Roles are not prefetched: discovery has normally
   already committed their artifacts, as it has a git collection's.

### Two pools, two resources

`--workers` bounds extraction, which is local work bound mostly by the
filesystem creating entries and only partly by CPU (see the measurement
below): an install or warm worker unpacks a tree in the same goroutine that
acquired it, and an install worker that ends up acquiring an artifact itself
does so inside that same bound.
`--download-workers` bounds the prefetcher instead: its background artifact
downloads and its cache-presence probe scan, both network-bound - a HEAD probe
or a streamed GET into a temp file, never an extraction. That is why
the download default is the larger of the two, and why it derives from the CPU
this process may use rather than from the node's core count.

The two defaults are sized from different budgets. The install default is
floored at 2 for latency hiding rather than throughput - an install worker can
block on an acquisition the prefetcher did not cover - at a cost of about 12%
on one CPU, and capped at 16 by memory: a worker holds at most one gzip reader
at a time, since manifest read, chain check and unpack run strictly one after
another, and pgzip's default four 1 MiB blocks make 16 workers a 64 MiB
block-pool budget. Extraction on ext4 was still getting faster at 12 workers,
so the cap must not drop below that. The download pool opens only the shape
probe's decompressor, one 64 KiB block, about 2 MiB at its default ceiling of
32, which is what lets it be sized from the network; sizing the probe like the
extractor would charge that pool against the install pool's memory. The
ceiling of 32 is half the idle connections kept per host, so the pool keeps
every worker's connection.

Both derive from `runtime.GOMAXPROCS(0)`, never `runtime.NumCPU()`, which
under a CFS quota (a Kubernetes `limits.cpu`, `docker --cpus`) still reports
the node's cores; only a cpuset narrows both. No test pins that argument,
since narrowing `GOMAXPROCS` in a test would race the package's parallel
tests, so a regression would size a 2-CPU pod on a 64-core node from 64. An
operator may exceed the defaults: the accepted `--workers` ceiling,
`max(procs, 2)`, bounds that flag only and must never bound
`--download-workers`, whose useful size is a multiple of the CPU count, and
`outdated` deliberately stays on `--workers`. The derived default must lie
inside the accepted range for every CPU count, because an exported but empty
`GO_GALAXY_WORKERS=` reaches the range check carrying the flag's own default,
and a default outside it would warn about a value nobody wrote. Filesystem
metadata writes limit extraction more than CPU does - creating 46,100 entries
took 12.8 s where walking them took 0.28 s, and APFS on a 12-core host peaks
at 4 workers - so on such a filesystem a `--workers` below the default is the
faster setting.

### Prefetch and handoff

The prefetcher scans cache presence in parallel, then downloads ahead of the
install workers in install-level order. Its download workers never touch the
collections tree or the extracted store - their dependencies carry a nil root and
a nil extract store by construction - while the presence scan ahead of them does
stat the tree, through the same `os.Root`, to skip a collection that is already
installed. An install worker waits on its collection's key and takes
ownership of the temp file, so nothing is downloaded twice and nothing is
reclaimed twice. A prefetch failure is a warning, not a run failure: the worker
falls back to acquiring the artifact itself.

The scan's answer is reused for the rest of the run. A key it found cached, and
therefore did not schedule, enters a presence set, and an install or warm
worker treats a key in that set as a cache hit without a second `Has` probe (a
signed HEAD on S3). The set is built once, before any consumer is dispatched,
and never mutated, which is the only reason it is read without a lock; and a
scan-time answer stays true only because nothing in the run writes a key the
set names. A scheduled key never enters it, since the prefetcher is about to
write that key, so after a failed prefetch the worker probes for itself; an
install worker commits only on the cache-miss arm the set gates; two
collections cannot share an artifact key, as neither a namespace nor a name may
contain the `-` the filename joins on; and the forced refetch after an eviction
bypasses the set. Recording a scheduled key there, or committing a key anywhere
else, breaks this. The scan itself is fail-open - a `Has` error schedules the
download - and uses only the cheap install-record check, since a wrong answer
costs a redundant download and the install worker's full skip check is the
real gate. A prefetch task does not re-probe before it commits, because it is
its key's only committer in the run: the key's consumer blocks until it
finishes.

Every task, a canceled one included, ends by recording its result and closing
its key's completion channel, so no waiting worker can deadlock. The consumer
must release the temp even when it skips the install, and closing the
prefetcher, after it cancels and joins its workers, reclaims every temp no
consumer claimed - a level never dispatched because an earlier one failed, or
a canceled warm - which would otherwise be left on disk.

The prefetch pool is joined before the lock is released, structurally rather
than by convention - the join is deferred inside a callee of the function whose
own defers release the lock - so a late worker can never commit to the cache
after the run stopped holding it.

The prefetcher is disabled outright under `--dry-run`, `--no-cache`, or with no
artifact store, rather than by threading a flag into each worker.

### Acquiring an artifact

In priority order: a prefetched temp file wins; otherwise, an artifact that is
already cached, whose metadata the resolver did not push, and in a run that is
not verifying signatures, is served from cache with **no metadata request at
all**; otherwise metadata is fetched and the artifact downloaded.

That middle path is the metadata-free fast path, and the third condition is what
a verifying run gives up: a server's own signatures ride on the same
version-metadata document, so keeping the shortcut would let a server-signed
collection pass vacuously on a cache hit.

When a cache hit does load its metadata and the load fails, the run warns and
goes on without it, carrying the fact so verification reports the metadata as
unavailable rather than the collection as unsigned. That arm alone raises
`ErrMetadataUnavailable`, the one metadata failure `prepareInstall` tolerates,
so any other producer of it would turn a cache miss's metadata failure into a
silent install; this is why an unbuildable metadata URL has a sentinel of its
own, `ErrMetadataRequestBuildFailed`.

The digest a collection is judged against follows a trust ladder, and the rule
behind it is *validate what crossed a trust boundary, never what this process
just computed*: bytes this process hashed itself are authoritative; a lockfile
pin forces a fresh hash of the file rather than trusting any recorded value;
only then are a server's declared digest and a cache sidecar consulted, each
validated for shape first.

Downloading has two arms. With an extracted store, one pass writes the temp
file, hashes it, and feeds an extraction through a pipe simultaneously. Without
one, the bytes are written and hashed, then probed for tar.gz shape - a check
the streaming arm answers by construction, and one that keeps an error page from
occupying a shared cache slot when a server declares no digest.

The shape probe answers only whether a file is gzip with a tar stream starting
inside it, and is no integrity check: it stops at the first header
`archive/tar` returns, accepts an empty tar as the extractor does, and applies
none of the extractor's entry, size or path rules. It misses truncation once
the surviving prefix still yields that first header, because pgzip returns a
truncated read that produced bytes as a short block with no error; completeness
and content rest on the sha256 and the full extraction. Its walk is bounded by
`ArchiveProbeMaxBytes` of decompressed bytes, since `x`, `L` and `K` meta
headers are consumed inside a single `Next` call, and crossing the bound is
reported as `ErrArtifactTarHeaderNotFound`, never also as
`ErrArtifactNotTarGz`: such a stream is gzip and tar, and that headline would
send an operator looking for an error page. The caller's cancellation or
deadline comes back as itself, not as a shape verdict.

An origin download - a Galaxy collection's or a url source's - makes up to four
attempts at the whole download (establish, stream, verify) under the one
artifact deadline, each failure classified in a fixed order that differs from
the Galaxy API's. Offline mode and the artifact deadline are terminal first,
ahead of a read stall, so a stall that raced the deadline spends no backoff on
a dead context; a read stall is then retried; the caller's cancellation or an
expired context is terminal; the content verdicts - a sha256 mismatch, an
oversized body, bytes that are not tar.gz, no tar header within the probe's
bound - are terminal even on a retryable status, since the same URL serves the
same bytes; and last, an attempt is retried on a retryable status or on no
response at all, a transport failure the API's predicate never retries, because
an artifact streams a far larger body over a longer-lived connection. Anything
else, such as a local filesystem error, is final. The terminal classes are
listed explicitly although the default-deny fallthrough would cover them, so a
reordering cannot start retrying a corrupt or hostile artifact. The deadline
sentinel is outside the evict-and-refetch class as well, so a deadline on a
cache hit never deletes the cached artifact.

A git collection takes the same path with one difference: it is normally a
cache hit, because discovery already committed its artifact. When it is not
(a dry-run discovery committed nothing, the cached artifact was evicted, a
`--frozen` run meets a commit no cache holds), the pinned commit is fetched
again and rebuilt, restricted to that one collection's identity; a rebuild
that yields a different identity is an integrity failure, never a
substitution. It carries no server digest and no signatures, and its
attribution is the identity the builder read from `galaxy.yml`, checked
against the collection being installed.

A url collection is normally a cache hit for the same reason. Its cache miss
re-downloads the pinned URL over the url client and enforces the pin twice
over the fresh bytes: the downloaded sha256 must equal the locator's, and
the manifest's identity must equal the collection being installed. Unlike a
git collection its pin travels as `col.SHA256` on every resolution, so the
digest is re-checked against the actual bytes on every install and warm,
cache hit included, not only under `--frozen`.

### Verify, extract, record

Two questions in order, both before anything is written into the collections
tree: the pin proves these are the bytes the lockfile names, and the signature
proves who published them.

A collection's signature gather is a pull: declared sources in file order,
then the server's blobs, one at a time, since 64 signatures of up to 1 MiB
each would otherwise sit resident per collection times the worker count; for
the same reason there is no run-level blob cache, and two roots naming one URI
fetch it twice. The cap of 64 counts every candidate consumed, duplicates
included, so one URI listed under many spellings cannot spend unbounded
requests, and a repeat is dropped by the sha256 of its bytes, which saves only
a key operation since verification counts distinct keys. The server's blob
list is copied up to the same cap while its uncapped count still feeds the
truncation warning, and indexing the gather across both lists stays in range
only because the two caps are one constant; they must not be changed
independently. One keyring and one policy serve every worker of a run without
cloning: nothing writes a loaded `Keyring` and `Verify` keeps its per-call
state local, so mutable state added to either - a lazy index, a cache of
verified keys - breaks that sharing. `Verify` reports a vacuous pass as a
result field rather than warning, since it knows no collection name; the
caller must print the warning, or the vacuous pass goes silent.

A signature source's grammar has one home, `signature.parseRequirementSource`,
behind three entry points: the validation `requirements` runs at load, the
`--offline` warning's check whether a source needs the network, and the fetch.
Every refusal belongs in that grammar rather than only in the fetch: a value
accepted at load and refused in an install worker is joined behind
`ErrInstallationFailed` and exits as an install failure instead of a usage
error, and a hostless http(s) URL left to `http.Client` fails as "no Host in
request URL", reported as an unreachable source - the transport class a CI
retries. The fetch keeps its own copies of the file checks only as a backstop.
Every message names a source by `helpers.URLForMessage` (userinfo, query and
fragment cut), computed before `url.Parse` so even an unparsable value can be
named; the fragment cut matters because `file:///tmp/a#b.asc` opens `/tmp/a`,
so the name printed is the file actually read.

Extraction resets the destination through `os.Root` - a rooted remove followed
by a rooted directory creation - and then hands the per-entry untar that
just-created directory as a plain path, deliberately unrooted. Rooting each entry
write was measured at 1.5x to 2.3x the cost for no marginal coverage, and the
reason it buys none is the reset immediately beforehand: nothing can be
pre-planted inside a directory this run just created. What `os.Root` is load
bearing for is the ancestors - `ansible_collections`, the namespace and the name
directories - which it refuses to traverse if any of them is a symlink out of the
tree. The root is established at the download path itself rather than one level
lower, for the same reason the extracted store establishes its own at the cache
directory.

`os.Root` reports a refused traversal with an unexported error ("path escapes
from parent", also what a `MkdirAll` through a symlinked `ansible_collections`
returns) that no caller can match with `errors.Is`, so the failure path of
every rooted write into the collections tree - opening the root, the
extraction reset, the `.info` sidecar writers - and into the roles tree, and
both dry-run probes, re-derives the class. It walks the path top down with
`Lstat` and wraps the original error in `ErrCollectionsPathEscape` (exit `5`,
with a hint to point the configured path at the real directory rather than a
symlink) at the first symlink component, or at the first component whose
`Lstat` fails with anything but not-exist or `ENOTDIR`, an unrecognized
failure being presumed an escape. `ENOTDIR`, a regular file blocking a
directory component, stays unclassified, so an ordinary naming conflict exits
`1`. Containment never depends on this walk, since `os.Root` has already
refused the operation, but the exit class does, and for the dry-run check of
`ansible_collections` it is the only signal a preview has.

Extraction resets the collection's version-scoped `.info` directories along with
its tree - every version's goes, and this version's comes back empty - and ends by
writing the extract-done marker into that fresh directory. Recording then adds
two more, in this order: the `GALAXY.yml` sidecar beside the marker, and the
store entry. The sidecar is written file by file rather than by resetting the
directory again, since that would erase the marker: each name is removed before
it is written, so a symlink or hardlink planted at it is severed rather than
written through, and anything at the directory's own name that is not a real
directory is replaced by one. `GALAXY.yml` carries exactly the keys of the
schema ansible-core validates it against, since ansible discards a document
with any other key; a git or url install's provenance - the commit, or the
sha256 of the fetched bytes - goes into `go-galaxy.yml` beside it instead,
which is what `outdated` tells the install's kind by.

A later run reads the marker, the sidecar and the store entry back to decide
whether to skip. The sidecar is parsed, not merely found: it has to name the
collection at the version being installed, so a truncated or foreign document
is not evidence of an install and the collection is installed for real. What
may still disagree is what earlier releases wrote: a `server` holding the
run's default rather than the resolving server, a document outside ansible's
schema, with the provenance key inside it or a null `signatures`, and - for
every sidecar they wrote - `signatures` in a different place among the keys.
The store entry has already been shown to name this collection and this
source, so the skip path writes `go-galaxy.yml` from that entry first, then
re-renders `GALAXY.yml` with the server corrected and every key outside the
schema dropped, and skips anyway. The order is what keeps the provenance: a
failed first write leaves the old document, which may hold the only copy, as
it was. These are the only writes a skipped install makes, and each happens
only when the file on disk is not already byte for byte what this tool
writes.

### Bounded recovery

A cache-resident artifact that fails its digest check, its manifest chain, or
its extraction is evicted and refetched exactly once. "Once" is structural, not
a counter: eviction sets a force-download flag, and the next iteration's guard
returns on any failure instead of evicting again, so the loop runs at most twice
and never recurses.

The loop has two arms, classified differently. A failure while preparing the
artifact, before verify and extract, is evicted only when it is a sha256
mismatch on a cache hit, which only the S3 artifact store raises at read time:
it records each object's sha256 as `x-amz-meta-sha256` user metadata and
compares the downloaded bytes against it on every fetch (an object with none
recorded is accepted unverified), while the local store checks no digest on
read. The comparison is exact lowercase hex, never case-folded, since that is
the only spelling any producer writes; a case-only mismatch is a mismatch, so
the refetch re-commits canonical metadata and clears a bad object in one online
run, where a folded match would let the digest through to fail every later run
as malformed. Every other preparation failure would recur against the same
entry or origin and surfaces without eviction, a deadline on a cache-hit read
included. A malformed recorded digest in particular - a server's declared one
or a cache sidecar's - is refused there as `ErrMalformedArtifactSHA256` (exit
`7`): let through to a cache-hit extraction, it would delete and refetch a good
tarball on every run, since the poisoned party is the cached metadata, which
eviction does not touch. The action arm, verify and extract, evicts on any
failure but the classes below.

Three classes are excluded because refetching cannot repair them: a
destination-side failure, where the fault is the tree rather than the artifact;
a signature source that was never obtained; and a verdict over the *set* of
signature blobs, which the same bytes would produce again. Evicting for any of
them would hand a hostile requirements.yml or server a deterministic delete and
re-download against the shared cache, per collection per run. Excluding the
verdict leaves one residual: a cached artifact swapped for unsigned or wrongly
signed bytes is not repaired automatically, but fails closed on every run
naming the collection, and `--clear-cache` is the remedy. Eviction is also
skipped outright when the artifact never came from a cache hit, and under
`--offline`, where deleting the only local copy with nothing to refetch it from
would be pure data loss. Eviction removes the tarball and its sidecar only -
never the extracted store, whose entries are content-addressed and remain
correct for every other project referencing them.

### Dry run

Under `--dry-run`, `install` and `warm` replace their install phase with
probes that are read-only mirrors of the decisions a real run makes, each
check placed where a real run meets the same failure, so the preview reports
the verdict and exit code the real run would. The probes run on the `--workers`
pool, each writing into the slot of its key's sorted index, so the report is in
key order rather than completion order, and its case order is the order a real
run reaches the outcomes: settled first, since a real run skips a settled
collection before any offline check or tree write; then not cached under
`--offline`, the real run's offline guard, which therefore outranks a probe
failure; then a probe's failure, a pin mismatch or a refused namespace write
that a real run meets only with the artifact in hand; then cached; then would
download.

`install`'s probe calls a collection settled through the install record and
the pure half of the extract-marker check, never through the real skip check,
which deletes a drifted marker while deciding to re-extract: a preview must
not change state by describing it. Settled comes before the pin, which is safe
because the install record already has to match the pin, and a real install
skips a settled collection without opening its tarball; the pin verdict and
then the namespace-directory probe follow, in the order a real install
verifies and extracts. `warm`'s probe never consults install state and checks
the pin before the extracted store's readiness, as warm itself does. A
collection is settled for warm only when both halves of warm's product exist -
the artifact cached and its tree ready in the extracted store under the
lockfile pin or, without one, the snapshot's warmed record - since with an S3
backend the artifact store is remote while the extracted store is always
local, so a fresh runner over a warm bucket has every artifact and no tree. A
collection with no nameable sha is reported as would warm, never guessed at;
learning the sha through a fetch would be a full object download on S3,
costing the preview what the run costs.

Whether an artifact counts as cached must mirror the real cache-hit predicate:
nothing is cached under `--no-cache` or with no artifact store, and a forced
refetch never applies to a preview; if the two drift, `--no-cache` against a
warm cache previews as cached while the real run downloads. Each probed
collection costs exactly one `ArtifactStore.Meta` call and no `Has` - on S3
both are the same HEAD - and that call also hands the pin verdict the recorded
digest. It relies on every backend's `Meta` reporting found exactly when `Has`
would, which each backend's tests assert; an error or a not-found reads as not
cached, neither a hit nor a would-fail, even when a degraded backend answers
found together with an error.

## The caching model

### Artifact cache: scoped by server, not by content

An artifact's key is a short fingerprint of the server base plus the escaped
filename, flat, with no directory nesting. Server scoping is the point: without
it, two servers publishing the same `namespace.name@version` would collide on
one slot and a cache hit would serve whichever landed first, indefinitely.

Content addressing was considered and rejected here for a mechanical reason: the
digest is not known until the download completes, and the cache-hit fast path
has to be answerable *before* any bytes are fetched.

A git source has no server base; its scope is the locator
`git+<url>#<subdir>@<commit>` the resolve produced, which is also what the
installed record and the resolved snapshot carry as that collection's source.
One string therefore moves the key with the commit, forces a reinstall when
the commit changes (the installed record's source no longer matches), and
lets cleanup delete the right artifact, without any of those consumers
knowing what a git source is: they compare sources, and a locator is a
source. For the same reason a requirements root whose `type` disagrees with
its source is refused before resolution: it must be typed `git` exactly when
its source is a `git+` locator and `url` exactly when it is a `url+` one,
because every one of those consumers dispatches on the locator's prefix alone,
never on the type.

Because the locator is persisted, its unambiguous parse is a format contract,
resting on three rules of `gitsource`: `Locator.String` always writes the `#`,
even for an empty subdir; a canonical URL can never contain a `#`, since the
URL grammar refuses a query or fragment and admits no `#` in a path; and no
subdir element may contain an `@`. `ParseLocator` therefore cuts at the first
`#`, takes the commit after the last `@`, which must be forty lowercase hex
digits, and accepts only the canonical spelling, a URL part that comes back
unchanged from `ParseURL` (else `ErrInvalidGitLocator`). Admitting a `#` in a
URL or an `@` in a subdir, or omitting the `#`, would make locators already
persisted in caches ambiguous.

A role's scope is the same locator with no subdir, `git+<url>#@<commit>`, and
its filename is `role.<name>-<version>.tar.gz`. The `role.` prefix is what
keeps a role and a collection built from one repository and commit apart
inside one scope: the text before the first `-` of a collection filename is a
namespace, whose alphabet has no dot, and here it always carries one.

A url source's scope is its own locator, `url+<url>#sha256:<hex>`: the pin
is the digest of the origin's own bytes rather than a commit, which is why a
url lockfile entry carries a real `sha256` where a git entry's is refused -
no rebuild stands between the origin and the artifact, so the digest holds
on every refetch. The download runs over a dedicated client
(`fetch.NewURLDownload`) that carries no Galaxy token and no relaxed TLS for
any origin, with the operator's Bearer binding re-decided on every redirect
hop; the artifact's identity is read from its own MANIFEST.json during the
pre-solve expansion (`expandURLRoots`), the same exact-pin road a git root
takes.

`helpers.ArtifactFilename` and `helpers.RoleArtifactFilename` are composed the
same way by every writer (install, warm, git and url discovery) and by
`cleanup`, which purges through `ArtifactKey(source, filename)` and ignores the
store's delete error, so a drift between the two compositions fails nothing
and silently stops cached tarballs from ever being purged.
`helpers.IsScopedArtifactKey` mirrors `ArtifactKey`'s output shape (twelve
lowercase hex characters, then a dot) and must change in lockstep with it,
since `cleanup`'s legacy-key sweep relies on it to skip a legacy key that
spells a live one (see [Clearing and cleanup](caching.md#clearing-and-cleanup)).

### Extracted store: content-addressed, materialized by hardlink

Unpacked trees live under a per-digest directory and are materialized into a
collections tree by hardlink, with a copy fallback across devices. A readiness
marker is written last and carries a **version tag rather than a bare sentinel**:
the readiness check runs before the per-digest lock, so a tree unpacked by an
older binary would otherwise be trusted verbatim and hardlinked everywhere.
Bumping that tag forces every pre-existing tree to be rebuilt once.

Every path the store creates, renames or removes resolves through an `os.Root`
established at the **cache directory**, never one level lower: opening a root
follows a symlink while establishing it, so rooting at the store subdirectory
would adopt whatever that name pointed at.

Whether a digest is trusted or re-checked is carried explicitly, and the zero
value is the verifying one: a digest read back from a record makes the store
hash the file before extracting under it, while a digest this process computed
over exactly those bytes does not.

`cleanup` builds the store's keep set from the persisted snapshot, never from
its scan of the disk: a project whose workspace is absent this run, the normal
state on an ephemeral CI runner, contributes nothing to a scan, and its
installed records are never pruned, so a set derived from disk would wipe the
trees it uses. The set holds the artifact digest of every installed record
except those this run removes as unreachable, every warmed entry still inside
its 30-day window, and the digests of every role that stays. The exclusion is
computed explicitly rather than read from a pruned snapshot, so a `--dry-run`
plan, which prunes nothing, matches a real run, and the warmed half never
consults it, since warm's intent does not depend on install reachability. The
warmed set is the only thing keeping a warm-only machine's trees alive, and
only `warm` writes it, on every warm, cache hits included, so each re-warm
restarts the window; `install` and `lock` must neither write nor prune a
warmed entry, since one written beside an install would keep its tree alive up
to 30 days after `cleanup` legitimately dropped that install. The sweep runs
only when the snapshot has recorded content (see [Snapshot](#snapshot)), and
its failure is printed rather than returned: the layer is rebuildable, but
silence would hide a containment refusal or a lost cache lock.

### Snapshot

One value holds the cached API responses, version lists, dependency maps,
install records, the dependency graph, the requirements spec, the last
resolution, the warmed set, the git pins - per `(url, ref, subdir)`, the
commit a git requirement resolved to and the collections it held - the url
pins (`url_pins`) - by URL, the sha256 a url collection requirement's tarball
resolved to and its MANIFEST.json identity and dependencies - and the two role
buckets: `installed_roles`, by install name, the record of each role on disk
(install path, locator, artifact digest, version, Galaxy name, dependencies),
and `role_pins`, by requirement line (`url\nref\n`, the git pin key with an
empty subdir, for a git role, `galaxy\nname\nrequested-version` for a Galaxy
role, `url\n<url>` for a url role), what that line resolved to (repository,
commit, version, the commit the v1 API recorded, the dependencies the meta
declared, and for a url role its URL and sha256). The local backend
bucket-maps it into a single BoltDB file - one atomic transaction rather than
twelve file writes - and the S3 backend marshals it as one gzipped JSON object.

Retention and redaction are applied at persist time, in the single copy path, so
both backends inherit one set of rules rather than each implementing its own.

The schema version is bumped for **any** change, including a purely additive
one, and the policy is drop-and-rebuild rather than field-level migration. The
reason is the shared cache: an older binary reading a newer snapshot must fail
loudly rather than silently ignore a bucket it does not know about.

A dirty flag records whether a run changed anything, so a run that only read can
skip the save entirely. Every write-locked method must set it, which
`internal/galaxy/store`'s own audit gates. The skip is
`cache.WithCleanSaveSkip`: `SaveStore` is a no-op for a non-nil store that is
not dirty, while a nil store passes through, so the local backend's refusal of
one is not turned into success. Because retention is applied only when a save
runs, a skipped save skips eviction too, and anything that must honor a
retention window checks age when it reads, as the extracted-store keep set does
for warmed entries. Entries read with no age check - a dependency map, and a
version list or API response served under an exact-version policy - can
therefore outlive the 30-day window across a series of clean runs. That is
accepted only because an exact-version answer names an immutable published fact;
a new read of mutable data must check age itself. One loss is accepted too: on a
run that has already failed, an abandoned prefetch worker can record an API
response after the dirty check, losing one reconstructible entry, which an
unconditional save racing that worker would lose as well.

The `content_recorded` stamp (see [What the directory
holds](caching.md#what-the-directory-holds)) is decided from the live store
before age eviction, so a save on which the only warmed record expires still
stamps it rather than erasing the evidence and leaving that tree unreclaimable.
Only installed collections, warmed entries and installed roles count - never
pins, resolution data or API caches, which a content-free `lock` writes - and a
new bucket that records content on disk must be added to `hasContentEntries`.

### Resolution replay

A collection resolve can be answered from the snapshot in two ways before
anything is solved. A whole replay happens when the requirements signature
equals the persisted one and every root is satisfied by the recorded
resolution; it makes no metadata request at all. Otherwise, when some roots
are unchanged and
others were changed or added, `tryIncrementalResolve` keeps each unchanged
root's subgraph from the snapshot with no request, solves only the changed
roots through a nested resolve that neither replays nor records, and records
the merged graph. If the two halves disagree on a shared collection's version
or dependencies, or the merged graph fails validation, it falls back to a full
solve over every root and keeps neither partial answer; an error from the
partial solve itself still fails the run. So editing or adding one requirement
does not move an unchanged root's open constraint to a newer version unless
the merge conflicts.

The signature is the sha256 of two header lines, `no-deps=<bool>` and
`servers=<signature>`, followed by the sorted per-root lines
`fqdn|constraint|source|type|signatures`. The server signature keeps the
effective list in order, since order decides first-match ownership, and hashes
each server's id, URL and only whether a token is set, never the token, since
the signature is persisted: a token rotation leaves it unchanged, while adding
a token, adding an unused server or reordering the list costs one cold
resolve. An unpinned root's source stays empty rather than taking the default
server, so "no preference" and "pinned to the default server" hash
differently. The stored spec records neither `--no-deps` nor the server list,
so the incremental path first recomputes the stored spec's signature under this
run's mode and servers and declines unless it equals the persisted hash;
without that check, a root added after a `--no-deps` run, whose snapshot holds
roots with no dependency edges, would silently skip the unchanged roots'
dependencies.

Git and url roots are expanded before the signature is computed, so it covers
their pinned locators; over the unexpanded roots, a `--clear-cache` run, which
drops the pins and keeps the resolution, would replay the old graph.
`--refresh` vetoes both reuse paths except under `--offline`
(`refreshBypassesSnapshot`), the offline-before-refresh precedence the cache
policy applies too. The exception is necessary: the resolution, the graph and
the requirements spec are persisted with no age filter while the API,
dependency and version-list entries they were solved from age out after 30
days, so an older cache routinely holds a resolution with no metadata behind
it, and a fresh solve on the offline transport could only fail.

A signature source's query is cut twice before the spec is persisted: by
`normalizeSignatures` where the spec and its signature are built, and again by
the snapshot's copy path on every save. The first cut cannot be left to the
second: the persisted hash would then cover the query while the persisted spec
does not, the incremental path's self-consistency check would fail on every
run, and it would silently stop working while replay and full resolve still
succeeded. Only a recorded resolution writes the hash and the spec together,
so a hash that disagrees with its spec costs one full resolve, never a
failure. A query-only edit to a source is not a root change, which is safe
because signature sources play no part in resolution and verification reads
the live, uncut source.

### Project registry

Keyed by project directory, recording the requirements file, the collections
path and the roles path of every project that has run against this cache. The
roles path is `omitempty`: a record written by a binary that predates roles
carries none, and cleanup reads its absence as "do not scan", the direction
that can only make a destructive pass do less. `cleanup` computes reachability
from it, which is why a registry that exists and fails to decode is an error
rather than an empty registry: reading it as empty would mean nothing is
reachable, and delete everything. A missing registry is simply empty.

The registry carries no schema version, so it evolves only additively: an
older binary reads a newer registry without complaint but drops every field it
does not know when it re-records a project. A field added later must therefore
be read with its absence taken as the conservative answer, as cleanup does for
the roles path. Both backends build a record through
`store.NewProjectRecord`, keyed by the directory of the absolute requirements
file, with each path resolved against that directory and an empty path left
empty rather than becoming the project directory itself, so the local file and
the S3 object keep one shape.

`--dry-run` skips registering the project, since that is the only persistent,
non-cache, non-reconstructible write in the startup path, and it feeds a
destructive command.

### The extract-done marker

A file named for the artifact's digest, holding a format tag and a tally:
entry count, directory count, and total byte size. Not a hash. It catches any
file or directory added or removed, and any file whose size changed; it
deliberately misses an in-place edit that preserves the file's exact byte
length, which is pinned by a test so the guarantee is never mistaken for a
stronger one.

A collection's marker lives in its version's `.info` directory, not in the
collection directory: `ansible-galaxy collection verify` reports any file there
that `FILES.json` does not list, and fails over it. A role's lives in the role
directory, where cleanup and the directory-ownership check look for it; ansible
has no verify for a role. Keeping it beside the version is what makes the sweep
of other versions' `.info` directories load-bearing rather than tidy: the tree
is not scoped to a version, so without the sweep a marker for a version no
longer installed would survive, and installing that version again would find its
record, its sidecar and that marker all in place, leaving the tally as the only
check - one a patch release that changes no file's length passes.

The digest is validated for shape before it is ever joined into a path, because
a leading `..` would otherwise fuse into the marker's constant prefix.

Verifying a marker never fails a run. Every negative outcome - a missing, legacy
or unparseable marker, a failed scan, a tally that drifted, a digest that is not
a sha256 - removes any marker there is and re-extracts. Drift and an unsafe
digest warn, so they reach stderr under `--quiet`; the rest log only under
`--verbose`, or every install upgraded past a format change would warn once per
collection. The marker is exactly
`go-galaxy-extract-1 entries=<n> dirs=<n> bytes=<n>` and a newline, read under a
256-byte cap and parsed strictly rather than with `fmt.Sscanf`, which accepts
trailing garbage; a format change must change the tag, so older markers are
re-extracted rather than misread. The tally walks the install through its
`os.Root` without following symlinks (a symlink is one entry with its `lstat`
size) and skips top-level names carrying the marker prefix, so a role's marker,
which lives inside the role tree, never counts itself. Writing a marker first
removes whatever holds its name, so a hard link into the shared extracted store
there is severed rather than written through. Only the two skip checks and the
extraction run this destructive verification: the dry-run probes use its
read-only half, and the prefetch scan checks only the record, the sidecar and
the marker's presence, so it never pays for the tree walk.

## The cache seam

`internal/galaxy/cache` declares two interfaces with deliberately different
concurrency contracts, and both implementations depend on the split rather than
merely tolerating it:

- **`Backend`** - state and locking. **Not** safe for concurrent use; its caller
  serializes every call, including the accessor that returns the artifact store.
- **`ArtifactStore`** - tarballs. Safe for concurrent use across *distinct keys*;
  concurrent commit and delete of the same key is undefined.

`Backend.Lock` returns the holder context described above. A backend whose lock
cannot be taken away returns the caller's context unchanged, so no caller needs
a per-backend branch - which is exactly what the local backend does, because
`flock(2)` holds while the process holds the descriptor and a contender fails
immediately rather than displacing the holder.

`internal/cache` is the only package that names a concrete backend. Both
backends classify their failures into the same small set of cache-backend
classes, so one operator mistake yields one exit code whichever backend was
configured.

Beyond the concurrency split, an implementation must honor what its callers
rely on. `ArtifactFile.SHA` may hold only a digest the backend computed over
the returned bytes while producing them, because install trusts a non-empty
one without re-hashing, even for a lockfile-pinned collection; a digest read
from a sidecar travels in `Meta["sha256"]` instead. The local backend always
leaves it empty, and the S3 backend fills it from the bytes it just downloaded
and verified. `ArtifactStore.Meta` is tri-state: not found with a nil error
means not cached, exactly as `Has` returning false does; found with an empty
map means cached without metadata; and a non-nil error makes found
meaningless. `Has` stays a method of its own because it sits on the install
hot path and the prefetch scan, where the local backend's extra sidecar read
would be wasted. The caller never cancels `Lock`'s holder context, since it may
be the caller's own; a backend that derives one cancels it from its release
closure, so a clean release leaves no child context behind. `Artifacts` may be
called only after a successful `Open`, where the S3 backend builds its store,
and `SweepTemp` only under the exclusive lock, so every temp it deletes
provably belongs to a dead run.

`Commit` returns the file the caller then reads and releases, and the backends
differ in what that is: the local one renames the temp into its slot and
returns the slot path with no `Cleanup`, while the S3 one uploads the temp and
returns that same temp path with a `Cleanup` that removes it, and its `Fetch`
always downloads into a fresh temp. Code above the seam therefore uses the
`ArtifactFile` it was given - never assuming the temp is gone, never reusing
the slot path - and runs its `Cleanup` exactly once. That is what lets a
prefetched artifact reach its install worker with no second object-store GET
on S3, while its bytes still pass the lockfile pin check and the
extracted-store ingest.

The fixed state-object ceiling is applied on the caller's side of the seam, by
`WithStateDeadline`, which run setup and `cleanup` wrap inside
`WithCleanSaveSkip`. Only `LoadStore`, `SaveStore`, `LoadProjectRegistry` and
`RecordProject` get a budget, their expiry normalized into
`ErrStateObjectDeadline`; the rest pass through unbounded on purpose. `Lock`
must stay unbounded: the distributed lock has its own timings, a legitimate
wait can outlast the budget, and the holder context it returns spans the whole
run, so bounding it would cancel every run holding the lock longer than the
budget. `ClearFiles` is unbounded because `--clear-cache`'s bulk delete grows
with the cache, the accepted residual being that a byte-dripped listing page
holds the lock for as long as the drip lasts. On the local backend the budget
is inert, since its state methods ignore their context. Wrapping a backend is
safe only because nothing type-asserts a `Backend` to a concrete type.

### The local backend

The local backend never opens the Bolt snapshot before its lock is held:
`Open` only creates the cache directory, `Lock` takes a non-blocking
`flock(2)` on `.go-galaxy.lock`, and the Bolt file is opened lazily on the
first `LoadStore` or `SaveStore`. That order is what makes a second run against
the same directory fail at once with `ErrAnotherInstanceIsRunning` (exit `8`)
instead of waiting on bbolt's own file lock; the Bolt open is still bounded by
`BoltOpenTimeout`, reported as `ErrCacheBusy`, as a backstop. The lock file is
only an anchor for the `flock`: it is opened with `O_NOFOLLOW`, so a symlink
planted at the path, by a restored CI cache archive for instance, is refused,
and nothing a crashed run left in the file can pose as a live lock. It must
never be unlinked, which is why `--clear-cache` keeps it: deleting it while one
process holds its `flock` would let another create a new inode at the same path
and lock that one. On a non-unix platform taking the lock always fails, rather
than running without mutual exclusion.

`classifyCacheFailure` splits a filesystem failure two ways, deliberately not
per errno: a permission failure as unusable (exit `2`), anything else - a full
disk, an I/O error, a vanished mount - as unavailable (exit `4`). An error that
already carries a verdict passes through untouched (`alreadyClassified`): the
two cache classes, the busy, corrupt and schema sentinels, an empty cache
directory (an operator's configuration mistake on either backend) and a plain
not-exist, which callers such as `Fetch` branch on and which already exits
`2`. That list is load-bearing, because `exitClasses` checks the network class,
which matches unavailable, ahead of busy, corrupt and usage: a sentinel the
store starts returning that is missing from the list is wrapped as unavailable
and silently exits `4` instead of its own code.

### The S3 client

The S3 client labels a failure `http.Client.Do` returned as a transport failure,
unavailable and retried, only while the request's own context is live, never by
the error's shape (see [S3 Cache](caching.md#s3-cache-optional) for why). Once
that context has ended - the caller canceled, or a state or artifact budget
threaded into the request expired - the raw error is returned unwrapped, so a
Ctrl-C classifies as an interrupt and a budget's expiry is relabeled by its own
deadline normalizer (see [Exit codes](#exit-codes)); a cancellation racing a
genuine transport error wins the same way. Only failures `Do` itself returns go
through this funnel: a body read failing after a `200` is never labeled by it.
A listing or batch-delete read failing that way is labeled unavailable where it
happens, on the same live-context condition, and the label never makes it
retryable.
An idempotent request is signed afresh for every attempt, so `X-Amz-Date` is
never stale, and a PUT body is reseeked first; a listing page's request is
single-shot, so the page's one retry budget covers both a retryable status and
a stalled body read.

The client signs and sends one and the same percent-encoded path: `requestURL`
computes `encodePath` once and uses it both as the SigV4 canonical URI and as
the wire path, and `awsURIEncode` is SigV4's `UriEncode` exactly - only
`A-Za-z0-9` and `-._~` literal, `/` literal in a path, every other byte, each
byte of a multibyte rune included, as uppercase `%XX`. Letting `net/url` build
the wire path instead, whose `PathEscape` leaves `+$&,;=:@` literal, makes a
key holding a reserved character fail with `403 SignatureDoesNotMatch`.
`X-Amz-Security-Token` is sent only when a session token is set, since a strict
endpoint rejects an empty one.

Every S3 sentinel carries at most one of the unavailable (`4`), unusable (`2`)
and busy (`8`) classes, which a test enumerates, since `exitcode.FromError`
would otherwise classify it by table order rather than by meaning. The split
is by where a failure was discovered, not by how permanent it looks.
Unavailable is any non-2xx the remote answered, a permanent-looking `403`
included, except the `404` and a conditional PUT's `412` consumed as control
flow; a listing or batch-delete body that breaks off or does not decode; a
transport failure; a bucket found missing after `Open` created it; a `409` to
a conditional PUT; and a lock wait that never observed a holder. Unusable, fixed
by a configuration change and never by a retry, is an endpoint that fails
`Open`'s conditional-write probe or answers a lock `HEAD` with no ETag, an
invalid endpoint, and any redirect, since SigV4 signs the host and the
canonical URI, so no redirect can yield a verifiable request. Busy is only a
lock wait that observed another holder. Outside all three, a lost lock carries
its own exit-`8` sentinel, an artifact digest mismatch is joined behind
`ErrSHA256Mismatch`, which the exit code checks ahead of every cache class, and
an artifact that vanished between `Has` and `Fetch` fails only that
collection's install, since a vanished entry is bookkeeping catching up, not
an outage.

### The S3 distributed lock

The lock is one object, `<prefix>/locks/cache.lock`, and its only authority is
the `X-Amz-Meta-Token` and `X-Amz-Meta-Deadline` headers a `HEAD` reads; the
JSON body mirrors them for a human and is never parsed (why it still matters
is in [S3 Cache](caching.md#s3-cache-optional)). An acquisition draws one
16-byte token from `crypto/rand`, with no weaker fallback, and keeps it across
every retry. It creates the object with `If-None-Match: *`; on `412` it `HEAD`s
the object and, only when `lockExpired` judges it expired, swaps it with
`If-Match` on the ETag that same `HEAD` returned, never deleting it on the way:
a delete-then-create reclaim cannot arbitrate, since two acquirers acting on one
expired object would each see it absent, each create it, and both believe they
hold the lock. Expiry is judged by the writer-recorded deadline, falling back
to `Last-Modified` age against the TTL, and timing it cannot interpret counts
as expired, so a corrupt object can never block acquisition forever. A `HEAD`
answered with no ETag is refused as unusable rather than downgraded to an
unconditional overwrite, since an empty `If-Match` sends no condition at all.
After either write lands, `claim` reads the object again and starts the
heartbeat only if it still records this token.

The client never retries a conditional PUT: a transport failure there cannot be
told apart from a success whose response was lost, and a retry of a write that
landed would see `412` and misread its own success as contention. The lock loop
is the sole retrier of conditional writes, and the answers are shaped for it. A
`409` is a lost race to back off from, not an observation of a holder. A `404`
on the `HEAD` after a `412`, or on the swap, means a holder released in
between, so the create is retried at once - at most eight times in a row before
the loop falls back to backoff, so a backend answering `412` while `HEAD`
reports no object cannot spin. When an attempt fails after a PUT that may have
landed - the create or the swap failing with anything but `412` or `409`, or
`claim` failing after either succeeded - it first runs `abandonLockObject`, a
best-effort, token-guarded release on a fresh context: without it an object
carrying this run's token and no heartbeat would block every other acquirer for
the full ten-minute TTL, twice the five minutes each waits before giving up.
An arm where the remote answered that nothing was written (`412`, `409`, a
`404` on the swap) abandons nothing. `abandonLockObject` never cancels the
holder context: no heartbeat runs yet, and cancel-cause is first-cancel-wins,
so a cancel there would preempt the more informative cause `acquireLock` sets
from the returned error. The in-memory double cannot lose an applied PUT's
reply, so no test reaches the two abandons that follow a failed PUT.

When the wait ceiling fires, `waitCeilingErr` decides in a fixed order. The
caller's own context error passes through unchanged, so a Ctrl-C stays `130`;
then an observed holder yields the busy class (exit `8`); only then does the
in-flight attempt's error yield "no holder observed" (exit `4`). Observation
must be tested first, since the ceiling lands inside a request about as often
as inside a backoff sleep, and the reverse order would report ordinary
contention as an unreachable backend. The in-flight error is wrapped with `%v`,
never `%w`, because the interrupt class is checked before every other and a
reachable context error would take its code. Observation accumulates over the
whole wait and is set only by a positive answer - a live unexpired holder, a
swap refused with `412`, a foreign token found by `claim` - never by an expired
holder or a `409`. While the ceiling is still live, any attempt error ends the
acquisition at once, unclassified, so a backend that answers only with
failures exits `4` immediately rather than waiting the ceiling out. An
attempt's result must never carry both a release and an error: the loop checks
the error first and would leak the heartbeat and the lock object.

The holder context derives from the caller's context, never from the wait
ceiling's, which would cancel every run holding the lock past the ceiling. The
heartbeat runs on its own background context and refreshes the deadline with an
unconditional PUT under the same token. A failed `HEAD` or PUT is transient and
retried on the next tick, a `404` included, so a holder whose object was
deleted keeps working and never re-creates it. A foreign or absent token is a
definitive loss, recorded twice and independently: an atomic flag makes release
return the lost-lock error and skip the delete, and canceling the holder
context with that error stops the run. The flag cannot be derived from
`context.Cause`, because release cancels the same context itself. Release must
cancel the heartbeat and join its goroutine before it cancels the holder
context: the join guarantees that a tick in the middle of its decision has
already recorded its loss cause, which first-cancel-wins then keeps readable by
`cache.LockLostError`. Hoisting that cancel above the join breaks this, and only
a statistical race test notices. Release runs its `HEAD` and `DELETE` on a
fresh context bounded by the release timeout, since the caller's may already be
canceled, and deletes only while the `HEAD` still sees this token, a missing
object counting as released. The `DELETE` itself is unconditional, the one
unarbitrated write left: a competitor's write landing during that round trip is
deleted anyway, bounded only by the token check of whichever run's heartbeat
later finds another token on the object.

## The lockfile

```yaml
server: https://galaxy.ansible.com
collections:
  - name: community.general
    version: "11.1.0"
    source: https://galaxy.ansible.com
    sha256: <hex>
    deps: [ansible.posix]
schema_version: 1
```

Written canonically - two-space indent, collections sorted by name, each
dependency list sorted - and atomically, so its hash is stable and
`go-galaxy hash` is deterministic. The hash is the SHA256 of exactly those
bytes, so the layout is part of the contract: a change to it changes every
lockfile's hash.

`server` is provenance rather than a source of truth: a source-less entry takes
its source from the consuming run's own configuration, not from this field. It
is still part of the file's identity, so a `server_list` reorder counts as drift
even when every pin is untouched.

Loading distinguishes exactly two outcomes - the file is not there, or it is
invalid - and three consumers depend on that dichotomy being exhaustive.
Absence is the bare `fs.ErrNotExist` (`lockfile.IsNotExist`); every other
failure - a file that cannot be read, does not parse, has a `schema_version`
missing or outside 1 through 4, or fails validation - wraps
`ErrLockfileInvalid`, and a new failure arm must wrap it too. Every caller that
cannot proceed without a lockfile - `install` and `warm` under `--frozen`,
`lock --frozen`, `tree` and `explain` - loads through `LoadRequired`, which
turns absence into `ErrLockfileMissing` naming the path, exit `6`; a bare
`fs.ErrNotExist` reaching the exit-code classifier would be claimed by the
usage class and exit `2`. `hash` and `outdated` call `Load` directly and fall
back only on absence, since a lockfile that exists but fails to load must not
hide behind a plausible cache key or report; `lock --dry-run` warns about such
a file and diffs against none, and the metrics report leaves the lockfile hash
out.

`File.validate` re-judges every collection entry on load: a unique name, checked
before any later message prints it so a newline cannot forge an output line, in
the Galaxy alphabet or, for a url entry, ansible's wider word rule for each
half, since that identity came from the artifact's own `MANIFEST.json`; an exact
version for every type, without which a lockfile carrying `"*"` would make a
`--frozen` install silently take the server's highest version; for a git entry a
source, ref and subdir that come back unchanged from the git grammar, a
lowercase forty-hex commit and no `sha256`; for a url entry a source that
round-trips the url grammar, a lowercase 64-hex `sha256` and no ref, commit or
subdir; and for a Galaxy entry none of those three. An entry type below its
minimum `schema_version` (git `2`, role `3`, url `4`) is refused as hand-edited,
while any `schema_version` from 1 through 4 at least as high as the content
needs loads. `Save` does not validate: that a written file loads again rests on
the builder, which checks every version with `helpers.IsExactVersion` and keys
entries by fqdn so no name can repeat.

`lock` is the command that manufactures the pin every later `--frozen` install
trusts, so it validates a server's declared digest for shape before writing it,
and validates each version before spending a metadata fetch on it - failing
closed under the whole-run exclusive lock rather than after buying work. An
empty digest is left alone, which keeps servers that publish no digests usable.

A git entry pins a commit instead of a digest:

```yaml
  - name: acme.app
    type: git
    version: "1.2.3"
    source: https://github.com/acme/app.git
    ref: main
    commit: 0123456789abcdef0123456789abcdef01234567
    subdir: collections/app
    deps: [acme.lib]
```

It carries no `sha256` because the artifact is rebuilt from the commit and the
gzip bytes of a rebuild depend on the toolchain; a digest over them would fail
a frozen install for nothing. A file holding at least one git entry is written
as `schema_version: 2` and a file holding none stays at `1`, decided from the
entries alone, so a project without git sources keeps producing a lockfile
every release reads, and an older binary meeting a git entry refuses the file
loudly instead of reading its repository URL as a Galaxy server. Under
`--frozen` a git root is checked against the entries locked from its
repository under its subdir (the root's own directory or an immediate child):
zero such entries, or a different ref, is a mismatch. The ref is compared as
the requirements file now spells it, so a changed ref is a mismatch even when
it resolves to the same commit, and a root that names its collection must find
that very collection among the matched entries, while an unnamed one is
satisfied by any of them.

A url entry is the opposite case and its `sha256` is required:

```yaml
  - name: acme.kafka
    type: url
    version: "0.24.0"
    source: https://github.com/acme/kafka/releases/download/0.24.0/acme-kafka-0.24.0.tar.gz
    sha256: <hex>
    deps: [acme.lib]
```

The artifact is the origin's own bytes rather than a rebuild, so the digest
holds on every refetch, and a frozen cache miss re-downloads the URL and
compares the fresh bytes to it. A file holding a url entry - a collection's
or a role's - is written as `schema_version: 4`, ranked over the role and
git schemas by the same content-decides rule. Under `--frozen` a url root needs
an entry locked from the same URL, and a `version:` it asserts must equal that
entry's version exactly: for either kind of source, `--frozen` means that what
was asked for has not changed.

A role is listed under its own key and pins a commit the same way:

```yaml
roles:
  - name: docker
    type: galaxy
    version: 7.4.1
    galaxy: geerlingguy.docker
    source: https://galaxy.ansible.com
    repository: https://github.com/geerlingguy/ansible-role-docker
    ref: refs/tags/7.4.1
    commit: 0123456789abcdef0123456789abcdef01234567
  - name: app
    type: git
    version: main
    source: https://git.example.internal/platform/ansible-role-app.git
    ref: main
    commit: 89abcdef0123456789abcdef0123456789abcdef
    deps: [base]
schema_version: 3
```

`name` is the install directory under `roles_path`; `version` is the tag,
branch or commit the requirement asked for (for `HEAD`, the branch it resolved
to) and is deliberately not held to semver, since a branch is a legal role
version - the commit is the pin. For a Galaxy role `galaxy` is the Galaxy
name, `source` the Galaxy server whose v1 API answered for it (recorded as
provenance the way the file-level `server` is, and the server `outdated`
asks; a pin replayed from a snapshot written before the server was recorded
falls back to the run's first effective server) and `repository` the GitHub
repository the v1 API pointed at, and `ref` the qualified ref the tag or
branch was fetched as (`refs/tags/<tag>`, or `refs/heads/<branch>` for a
role the server lists no tags for), so a branch sharing a tag's name can
never be fetched in its place; for a git role `source` is the repository
and `ref` is the ref as the requirement spelled it. `deps` are the install names
of the roles the run installed for its meta. No `sha256`, for the reason a
git entry has none. Roles are sorted by name and each `deps` list sorted, like
the collections. A file holding at least one role is `schema_version: 3`, one
holding a git entry and no role stays `2`, one with neither stays `1`, decided
from the entries alone, so a project without roles keeps producing a
lockfile every release reads, and an older binary meeting a role refuses the
file rather than installing the collections and silently skipping the roles.
Every role field is re-parsed on load rather than trusted - the repository
through the git URL grammar, the ref through the ref grammar, the commit as
forty hex digits, the names through the role alphabets - since a lockfile is
repository content. A url role's entry is `type: url` with the tarball URL as its
source, the origin bytes' `sha256` as its pin (required, where a git or
Galaxy role's is refused), no ref, commit, galaxy or repository, and the
version as the label asked for or the sha's first twelve hex digits; a file
holding one is `schema_version: 4`.

`lockfile.Compare` is what `lock --frozen` gates on and what `lock --dry-run`
reports, and its contract is that `Compare(a, b).Empty()` holds exactly when
`a.Hash() == b.Hash()`. That holds because it compares every field of an entry
and of a role entry except the name it keys them by, plus the file-level
`server`; `schema_version` needs no comparison, since the hash re-derives it
from the entries. A field added to `Entry`, `RoleEntry` or `File` must
therefore join those comparisons (and `comparedFieldCount` or
`comparedRoleFieldCount`), or `lock --frozen` passes a lockfile `lock` would
rewrite. Deps compare as a multiset, ignoring order but not duplicates, to
match the hash, which sorts them and never deduplicates; a deps-only change is
drift, because a frozen install rebuilds its install graph from them. `Compare`
reads a nil file as one with no entries, so a caller tells a missing lockfile
apart itself - `lock --frozen` fails closed through `LoadRequired`, the dry
run's baseline reports everything as added - and it passes entry values through
verbatim, leaving hostile text to the printer to neutralize.

## The HTTP clients

`fetch.New` builds the shared client, which the S3 cache backend is handed as
well. It stacks the read watchdog over the token transport over a TLS
dispatcher over `*http.Transport` pools, and sets no `http.Client.Timeout`,
which would cap total transfer time: `ResponseHeaderTimeout` bounds the wait
for the first byte and the watchdog bounds each body read. `NewOffline` fails
every request with `ErrOfflineMode` before any auth or TLS layer runs. The
signature-source client (`NewUnauthenticated`), the git client (`NewGit`) and
the url client (`NewURLDownload`) are built with no server configuration at
all, so none of them can give an origin a Galaxy token or relaxed TLS - by
construction, not because a caller remembered to pass an empty list. The
command layer reveals the Galaxy and url tokens into fetch's own types
immediately before construction, the only place either is revealed.

A server's `validate_certs=false` builds a second transport with verification
off, and the dispatcher sends a request there only when its origin is exactly
that server's; a redirect target, a download host, the S3 origin and every
other server go to the verifying pool. It stays two pools chosen per request
rather than one transport with a per-address `DialTLSContext`, for two
reasons: `http.Transport` keys idle connections by proxy, scheme, address and
HTTP/1-only, not by TLS configuration, so one pool serving both policies would
be correct only through routing discipline; and setting `DialTLSContext`
switches off ALPN HTTP/2 negotiation for every origin. The token transport and
the dispatcher stay separate types so a revealed token and
`InsecureSkipVerify` never share a struct, and both pools come from one
constructor so their dial, pool and timeout settings cannot drift apart.

The watchdog runs each round trip on a context derived from the caller's,
because a body read honors the round trip's context. Its body arms a timer on
the first read, resets it before each later one and stops it after; when the
idle budget passes with no progress, it marks itself fired and cancels the
derived context to unblock the read. That read reports `ErrReadStalled` only
while the caller's own context is still live, so a genuine cancellation - even
one landing after the watchdog fired - still surfaces as `context.Canceled`.
The body is deliberately unsynchronized, since a lock would tax the download
path: unlike a raw response body it must not be closed while a read is in
flight, and reads must not run concurrently. Every caller reads a body to
completion and then closes it, and aborts a transfer by canceling the request
context, never by a concurrent `Close`.

## Operator output

`output.Printer` has three tiers. `Printf` is transient progress: dropped under
`--quiet`, and drawn as the spinner's suffix on a terminal. `Debugf` and
`DebugSincef` print only under `--verbose`. The rest always print -
`PersistentPrintf`, `Okf`, `OkVersionf` and `Updatef` on stdout, `Errorf`,
`ErrorVersionf` and `Warnf` on stderr. `Updatef` is its own verdict, for a
subject that is intact but superseded: a success mark would say there is
nothing to do, and a failure mark that something broke.

`internal/progress` is the one `Printer` and, under `--verbose`, also the
standard `log` package's sink (`runCollectionCommand` discards `log`
otherwise), so whatever a dependency logs is sanitized before it is printed.
Every byte it writes goes through `writeLine`, whose payload fields are
`safeout.Text`: the payload - a caller's format and arguments, a cause, a
version, a logged line - goes through `safeout.Clean` first, and the package's
own decorations (colored markers, the debug and timing prefixes, the version
tag) are added afterward and never cleaned, so a decoration's escapes survive
and no payload is cleaned twice. That ordering is why `OkVersionf` and
`ErrorVersionf` take the version, and `ErrorVersionf` the cause that ends the
line, as parameters of their own: a colored version formatted into the message
would have its escapes replaced with `U+FFFD`. The package-level `Okf` and
`Errorf` own no `Progress` and resolve color per call; `main` prints a run's
final error through `progress.Errorf`, so that line carries the same marker as
every other failure in a redirected log.

`internal/safeout` marks cleaned text with a type of its own, `Text`. Any typed
string - a plain `string`, another defined string type, a `~string` type
parameter - needs an explicit conversion, `Clean` or a visible `Text(...)`
cast, so run-time input cannot reach a `Text` parameter such as the spinner's
suffix uncleaned; an untyped string constant is still assignable, control
characters and all, and that gap is closed only by review. `IsUnsafeRune` is
the single definition of the unsafe set: `Clean` consults only it and
`helpers.IsPathElement` rejects every rune it reports, so a codepoint class
added there reaches `Clean`, `NewWriter` and `IsPathElement` together.
`NewWriter` sanitizes each `Write` on its own - no split lets a control
through, but a multi-byte rune split across two writes prints as one `U+FFFD`
per byte - so its callers, `tree` and `explain`, whose lines carry box-drawing
runes, write whole formatted lines.

The spinner draws on `os.Stdout` directly, never through a `Progress` stream,
so its frames bypass `writeLine`. It is gated on stdout being a character
device, which means a stdout redirected to the null device counts as a terminal
and gets frames and color. Each frame - autowrap off, erase, glyph, suffix,
autowrap on - is handed over in one write, with the suffix cut at its first
newline (the one line-affecting byte `Clean` keeps) so a one-line erase clears
it; a suffix wider than the terminal is truncated. Autowrap is off for one
frame's bytes, never for the run, because an exit that runs no deferred
`Close` - notably SIGQUIT, left unhandled so its goroutine dump reaches the
same terminal - must not leave the terminal unable to wrap. A render goroutine
draws only while the spinner's stop channel is still the one it was started
with, so one that wins the lock after `stop` wrote the restore sequence writes
nothing; `stop` must not wait for that goroutine, which takes the same lock to
draw. `Progress` stops and restarts the spinner around every line it prints,
which is why `Close` drops the spinner rather than merely stopping it: a kept
one would be restarted by the next line, hiding the cursor with nothing left to
stop it.

## Exit codes

What each code means is in [Exit codes](exit-codes.md); this is how a run
arrives at one. A run prints at most one line per failure, and `handleResult`
decides which. A caught signal wins and prints nothing; otherwise the error urfave handed `ExitErrHandler` is
classified and printed; otherwise a bare error from `app.Run` is a flag-parse
failure, which carries no go-galaxy sentinel and always exits `2` (the flow is
drawn in [Command flows](commands.md#process-entry-and-exit)). urfave reports
an argv flag failure itself on the root's `ErrWriter` but returns an
environment-sourced one (`GO_GALAXY_WORKERS=abc`) without printing anything,
so the root's `ErrWriter` is wrapped in a recorder and the bare error is
printed only when nothing was written there. The recorder has to stay on the
root's `ErrWriter` rather than move to `OnUsageError`: urfave resolves
`ErrWriter` through the root for every subcommand, but reads `OnUsageError`
from whichever command was parsing. go-galaxy itself never writes to
`ErrWriter` - its error line goes to stderr through `progress.Errorf`, the
version to the command's writer - which is what makes a recorded write mean
urfave told the operator.

`exitcode.FromError` walks `exitClasses` top to bottom and returns the first
match, so the table's order is the exit-code contract, and a test pins it:
interrupt `130`, integrity `7`, signature `10`, lockfile `6`, a server-supplied
URL carrying userinfo `5`, install `5`, network `4`, cache busy `8`, cache
corrupt `9`, resolution `3`, usage `2`, else `1`. Per-collection failures
arrive aggregated: the failure summary joins a headline -
`ErrInstallationFailed` for `install` and `warm`,
`ErrLatestVersionLookupFailed` for `outdated` - with every cause, and
`errors.Is` walks the whole tree. A class above the headline's own therefore
keeps its code whether its sentinel arrives bare or joined, while every class
below collapses to the headline's; [Exit codes](exit-codes.md) spells out what
that means per failure, a failed snapshot save joined behind the primary error
included. A usage or resolution failure keeps its code only when its producer
raises it before per-collection work starts, as the refusal of a non-exact
version is. The userinfo class exists for uniformity - without it such a
refusal would exit `1` bare, `4` behind the lookup headline and `5` behind the
install one - and cache busy and cache corrupt sit above usage because usage's
`fs.ErrNotExist` arm matches any tree wrapping a not-exist cause.

An error must carry the sentinel of exactly one class, or the check order
rather than its meaning decides the code, so a producer whose cause carries
another class's sentinel rewraps it at the source: the S3 state-object reader
turns `ErrResponseTooLarge`, a network sentinel, into `ErrStateObjectTooLarge`
(exit `9`) with the cause rendered by `%v`, and an empty gzip member into
`ErrCorruptStateObject`. [Sentinel errors and
exit codes](development.md#sentinel-errors-and-exit-codes) lists these
reclassifications and why a new sentinel needs both a predicate and a table
row.

**Interrupts.** The interrupt class is checked first and matches
`errors.Is(err, context.Canceled)`, so an error may leave `context.Canceled`
reachable only when it is the caller's own cancellation passed through
unchanged. That is why a sentinel ending work through a context this program
owns - `ErrReadStalled`, whose watchdog cancel genuinely yields
`context.Canceled`, and the artifact, metadata, state-object and
signature-fetch deadlines - renders its cause with `%v` (the rule is in
[Sentinel errors and exit codes](development.md#sentinel-errors-and-exit-codes));
switching one to `%w` makes a remote-driven stall exit `130` as if the operator
pressed Ctrl-C, even one joined behind the install headline beside an
unrelated deadline. The one sanctioned `%w`, `ErrSignatureSourceUnavailable`,
strips the `*url.Error` (whose message would print the full request URL) and
wraps the inner cause, so a Ctrl-C during a signature fetch still exits `130`.
A bare `context.DeadlineExceeded` is a
transport failure, `4`, not an interrupt. `cache.LockLostError` flattens in the
other direction: it renders the run's error behind `ErrCacheLockLost` with
`%v`, so no other class can match through it and lock loss outranks every
class, integrity included, by mechanism rather than by table position. It
returns the error unchanged when the parent context is canceled, so an
interrupt stays `130`, and when the holder's cause is not a lost lock, which
keeps the local backend, whose holder is the parent context itself, inert
without a per-backend branch.

**Deadlines.** `cache.deadlineError` (behind the metadata and state-object
deadlines), `collections.artifactDeadlineError` and
`collections.signatureDeadlineError` relabel an error with their budget's
sentinel only when their own budget context expired while the parent is still
live. A parent canceled or expired first leaves the error untouched - the
derived context inherits the parent's expiry, so its own `Err()` alone cannot
tell the two apart - and an error already carrying the sentinel is returned
unchanged, so normalizing inside a retry and again around the loop never
doubles it. `deadlineError` adds a causal check, that the error itself carries
a context error, so an HTTP status arriving in the instant the budget expires
still routes by status in the server walk (a `404` moves to the next server, a
`401` or `403` is an auth failure, a retryable status an unavailable server),
and a state-object verdict such as a corrupt registry, a size cap or a schema
refusal never collapses into a deadline. `artifactDeadlineError` keeps an
ambient shape instead - context state plus explicit exclusions for a digest
mismatch and an unusable backend - and the two are not to be unified. The
signature budget covers only the gather and the key checks interleaved with
it: reading the manifest and verifying its chain do no network I/O and run
under the caller's context, since nothing relabels their context error and a
slow disk would otherwise end the phase with a bare `context.DeadlineExceeded`.

**An interrupted run keeps its tail.** On cancellation the worker-pool dispatch
loops (`runInstallLevel`, `warmCollections`, `warmRoles`) stop handing out
work, while every worker already started is let finish and joined, so none is
still writing after the lock is released; one caught mid-extraction stops at
its next read and writes no extract marker. They return nil or a plain failure
summary, never `ctx.Err()`, because a non-nil return would skip the command's
tail, which must still save the snapshot and write metrics; the interrupt exit
code comes from the caught signal in `main`. The local backend's save does not
observe the context, so there it lands; the S3 save runs on the canceled
context and fails with it. One consequence is visible: when the cancellation lands between two
dispatches and every started worker succeeds, the run prints its ordinary
success line and still exits with the signal's code. `runInstallLevel` defers
its join ahead of the dispatch loop, so every return path - the guard against a
key missing from the plan included, which can trip after earlier keys of the
level were dispatched - joins the in-flight workers before the lock is
released, where a leaked worker would mutate the snapshot or commit to the
artifact cache unlocked. A level in which any collection failed ends the loop:
no later level and no role is attempted.

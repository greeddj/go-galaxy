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

`internal/galaxy/infra` is the per-run container: the printer, the shared HTTP
client, the clock, the temp-dir accessor, the metrics counters, and four
test-only deadline overrides that are reachable through accessors so a nil
container or a non-positive override falls back structurally. Nothing wires
those overrides to a flag, an environment variable or an ansible.cfg key.
Extend `Infra` rather than adding a global or widening a signature.

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
than continuing to install and persist without exclusivity. Granularity is one unit
of work per worker - an artifact already in hand still finishes extracting,
since neither the untar nor the store's rename is interruptible - but every
write to the shared cache ends with the context.

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
`installed_roles` record.

**Lock and frozen.** `lock` resolves roles in the same run and renders each as
a `RoleEntry` pinned by commit; under `--frozen` an `install` or `warm` takes
its roles from the lockfile with no network, checking each `roles:` entry as
written against its locked line (same source - the Galaxy name or the
repository - and same ref, and for a Galaxy role with a version asked for, that
version), and `lock --frozen` diffs the fresh role list against the file.
`outdated` asks the remote what a git role's ref points at now, and asks the
v1 API which tag is highest for a Galaxy role, comparing by name.

**Cleanup.** The registry records each project's `roles_path`; `cleanup`
scans only that directory, through an `os.Root` at it, and indexes only a role
directory carrying the extract marker, joined with the `installed_roles`
record for its path when one exists. Reachability is the project's `roles:`
roots plus, transitively, the recorded dependencies of every installed copy;
an unreachable role is removed through a fresh root at its roles path, its
artifact and record with it, and the extracted keep set takes the digests of
every installed role that stays.

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
   identifier, a non-exact version, or a duplicate key.
6. Check the post-condition that every requirements root came back resolved,
   which is what catches a solve silently dropping one.
7. Compute install levels by topological layering. This happens **before** the
   prefetcher starts, so a dependency cycle surfaces before any prefetch worker
   exists, and the level assignment can order the prefetch queue.
8. Start the prefetcher. Roles are not prefetched: discovery has normally
   already committed their artifacts, as it has a git collection's.

### Two pools, two resources

`--workers` bounds extraction, which is CPU-bound: an install or warm worker
unpacks a tree in the same goroutine that acquired it, and an install worker
that ends up acquiring an artifact itself does so inside that same bound.
`--download-workers` bounds the prefetcher instead: its background artifact
downloads and its cache-presence probe scan, both network-bound - a HEAD probe
or a streamed GET into a temp file, never an extraction. That is why
the download default is the larger of the two, and why it derives from the CPU
this process may use rather than from the node's core count.

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

Three classes are excluded because refetching cannot repair them: a
destination-side failure, where the fault is the tree rather than the artifact;
a signature source that was never obtained; and a verdict over the *set* of
signature blobs, which the same bytes would produce again. Eviction is also
skipped outright when the artifact never came from a cache hit, and under
`--offline`, where deleting the only local copy with nothing to refetch it from
would be pure data loss. Eviction removes the tarball and its sidecar only - never the extracted store, whose entries are
content-addressed and remain correct for every other project referencing them.

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
source.

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

### Snapshot

One value holds the cached API responses, version lists, dependency maps,
install records, the dependency graph, the requirements spec, the last
resolution, the warmed set, the git pins - per `(url, ref, subdir)`, the
commit a git requirement resolved to and the collections it held - and the
two role buckets: `installed_roles`, by install name, the record of each
role on disk (install path, locator, artifact digest, version, Galaxy name,
dependencies), and `role_pins`, by requirement line (`url\nref\n`, the git pin key with an empty subdir, for a git
role, `galaxy\nname\nrequested-version` for a Galaxy role), what that line
resolved to (repository, commit, version, the commit the v1 API recorded, the
dependencies the meta declared). The local backend bucket-maps it into a
single BoltDB file - one atomic transaction rather than twelve file writes -
and the S3 backend marshals it as one gzipped JSON object.

Retention and redaction are applied at persist time, in the single copy path, so
both backends inherit one set of rules rather than each implementing its own.

The schema version is bumped for **any** change, including a purely additive
one, and the policy is drop-and-rebuild rather than field-level migration. The
reason is the shared cache: an older binary reading a newer snapshot must fail
loudly rather than silently ignore a bucket it does not know about.

A dirty flag records whether a run changed anything, so a run that only read can
skip the save entirely. Every write-locked method must set it, which
`internal/galaxy/store`'s own audit gates.

### Project registry

Keyed by project directory, recording the requirements file, the collections
path and the roles path of every project that has run against this cache. The
roles path is `omitempty`: a record written by a binary that predates roles
carries none, and cleanup reads its absence as "do not scan", the direction
that can only make a destructive pass do less. `cleanup` computes reachability
from it, which is why a registry that exists and fails to decode is an error
rather than an empty registry: reading it as empty would mean nothing is
reachable, and delete everything. A missing registry is simply empty.

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
invalid - and three consumers depend on that dichotomy being exhaustive. An
entry's version must be exact: without that check, a lockfile carrying `"*"`
would make a `--frozen` install silently take the server's highest version.

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
zero such entries, or a different ref, is a mismatch.

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
git schemas by the same content-decides rule.

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

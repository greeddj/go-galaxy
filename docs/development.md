# Development

Everything here is reproducible from a checkout with a Go toolchain, `git` and
[just](https://github.com/casey/just); `just lint` additionally needs
golangci-lint installed at the pinned release. The test suite needs nothing
else - no network, no container runtime, no Python. Only the benchmark harness
does.

## Running the tests

```bash
just test      # go test ./...
just check     # go vet, staticcheck, govulncheck, fieldalignment, actionlint
just lint      # golangci-lint, at exactly the pinned release
just fix       # go fix + fieldalignment -fix
just deps      # go mod tidy && go mod vendor - after any dependency change
just build     # check + lint + test, then dist/go-galaxy and dist/go-galaxy-benchmark
```

To reproduce what CI runs:

```bash
go vet ./... && go tool staticcheck ./... && go tool govulncheck ./... && go tool fieldalignment ./...
go tool actionlint -shellcheck= -pyflakes=
go test -v -race -coverprofile=coverage.txt ./...
```

The four `go tool` binaries come from the `tool` directive in `go.mod`, so
neither the Justfile nor CI installs them separately. Their own dependencies
resolve in the same module graph as the product's, so `go get -u tool` can move
one past what its tool compiles against: `go.yaml.in/yaml/v4` is required only
by actionlint, its `v4.0.0-rc.6` changes API actionlint v1.7.12 compiles against, and it
stays at the `v4.0.0-rc.3` actionlint names until a release of actionlint moves
it. Run the suite with `-race` before considering any concurrency work done.

actionlint's own external linters are switched off in both spellings rather
than left to autodetection: it shells out to shellcheck and pyflakes when they
are on `PATH`, the CI runner has shellcheck and your machine may not, and a
check that fires only in CI is one nobody can reproduce before pushing.

One package, one test, one subtest:

```bash
go test ./internal/galaxy/archive/
go test ./internal/galaxy/archive/ -run 'TestProbeTarGzUsesTheProbeSizedDecompressor'
go test ./internal/gzipstream/ -run 'TestPgzipUsesSeeEveryConstructionThatReachesAReader/a_dot_import'
```

Go benchmarks live beside the code they measure, in `internal/galaxy/solver`,
`internal/galaxy/archive`, `internal/galaxy/manifest` and
`internal/galaxy/collections`:

```bash
go test ./internal/galaxy/solver -bench . -run '^$'
```

Four fuzz targets run their seed corpora under an ordinary `go test`; going
past the seeds is manual, with no CI hook:

```bash
go test ./internal/galaxy/solver -fuzz FuzzSolve -fuzztime 60s
```

**Do not run the suite as root.** Several tests assert that a read, a write or
a removal is refused by file permissions, and they skip rather than fail when
the process is not bound by those permissions - so a root run, which is the
normal case inside some CI containers, silently loses that coverage rather than
reporting it.

## CI

`.github/workflows/ci.yml` runs on pushes and pull requests against `main`, and
is callable so the release workflow gates a tag on the same checks rather than
on a second, drifting copy of them. One job, six steps: checkout, set up Go from
`go.mod`, the five `check` commands, golangci-lint at the pinned release,
`go test -v -race -coverprofile=coverage.txt ./...`, and a coverage upload that
does not fail the build.

`.github/workflows/release.yml` fires on a `v*` tag, runs that same CI job
first, then GoReleaser: per-platform binaries and archives, `checksums.txt`,
keyless cosign signatures, SPDX SBOMs, multi-arch images, a Homebrew cask
committed to `greeddj/homebrew-tap`, and a build-provenance attestation. See
[Security](security.md#verifying-a-release) for the verifying side of that.

Three of those treat a prerelease tag as a prerelease rather than as a release:
the GitHub release is marked one, the Homebrew cask is not written to the tap,
and the `latest` image tag does not move. Only the version-tagged image is
published for an `-rc`.

Because that first job is a full gate, `.goreleaser.yml` runs no `before`
hooks: a plain `go test ./...` inside the release job would be re-testing a
tree that had already passed the same suite with `-race` and every static
check, minutes earlier. The release also moves the lightweight `v1` tag, so
`uses: greeddj/go-galaxy@v1` resolves - the one tag in this repository that
does not fix a state, and the reason the `Justfile`'s `git describe` carries
`--match`. A prerelease tag does not move it.

`.github/workflows/action.yml` is the only test the composite action at
`action.yml` has, because what that action does - download a published release
and run it - exists nowhere inside the module for `go test` to reach. It
installs a real collection from galaxy.ansible.com on Linux and macOS, so it is
the one thing here that depends on somebody else's service, and it runs on a
change to the action or to itself rather than on every push.

GoReleaser groups the release notes out of commit subjects, so the subject line
is the only thing deciding whether a change is published and where. Those rules
belong to whoever is writing the commit rather than to whoever is reading this,
and they live in [Contributing](../CONTRIBUTING.md#commit-subjects) - one copy,
beside the tag conventions they go with, rather than a second one here to drift
against `.goreleaser.yml`.

## The repository audits itself

Every gate is an ordinary test, run by `go test ./...` like anything else. Three
test-only packages - `internal/proseaudit`, `internal/lockaudit`,
`internal/ciaudit` - hold nothing but gates, and nothing imports them; five
more gates are audit files sitting inside the package they gate.
Either way they need no Justfile target and no CI step, and the only cost they
carry is three depguard entries for `go/ast`, `go/parser` and `go/token`.

They are also **anti-vacuous**: a gate that names a function or a file fails
when it cannot find it, rather than skipping, so a rename can never quietly
disable one. There is one deliberate exception - the dash gate and the
comment-length gate skip when `git` cannot enumerate the tree, since a module
extracted into the build cache has no committed text to check.

These are the checks that fail a build in a way the error message alone does not
explain, so each is worth knowing before it fires.

### `internal/proseaudit` - hyphen-minus only

No committed file may carry an em dash (U+2014) or an en dash (U+2013). The
gate reads the contents of committed files; the same rule applies to commit
messages as a house rule rather than as something it can check. Files are
enumerated through `git ls-files` rather than by walking the filesystem, because
the rule is about committed text: a walk would sweep in working files that are deliberately
outside it, and would silently stop covering tracked prose under dot-directories.

Everything tracked is in scope, `.github/`, `.golangci.yml` and every document
in `docs/` included, and a new file enters scope the moment it is staged. The
failure names each occurrence by file and line. To satisfy it, type `-`. A test
that legitimately needs one of the two characters spells it as bytes or as a
`\uXXXX` escape.

### `internal/proseaudit` - comment line citations

A comment anywhere in the module may cite a test file's line
(`foo_test.go:NNN`) only when that line is one `go test` could attribute a
failure to, and may never cite a production file's line at all - reference
production code by identifier instead. The cited file is resolved by its base
name in the citing file's own directory, never in another package, and for a
range every line must qualify. Unlike the dash gate, this one walks the
filesystem rather than asking `git`, skipping only `vendor/`, `testdata/` and
dot-directories: an untracked `.go` file is audited too, and every `.go` file
must parse.

A failure-attributable line is one of exactly three shapes: a call to
`Fatal`/`Error`/`Log`/`Skip` and their formatting variants on a testing value,
matched across the whole call so any line of a multi-line call qualifies; a call
to a same-package helper that calls `t.Helper()`; or any function's declaration
line. That leaves one known blind spot: a citation that drifted onto an argument
line of a multi-line failure call still passes. Narrowing the match would fail
correct prose, because the line `go test` reports for such a call depends on
its layout.

The consequence that catches contributors out: adding an import or a helper to a
test file shifts its line numbers and breaks citations elsewhere in the same
package. Either re-run and update the number, or replace the citation with an
identifier.

Note the interaction with `fieldalignment`: a `just fix` rewrite can reorder
struct fields in a test file and shift the very lines other comments cite.

### `internal/proseaudit` - comment blocks of at most three lines

No comment block in a tracked Go file, shell script, YAML file, `Justfile`,
`Dockerfile` or `.gitignore` may run past three lines. In a Go file a block is
a comment group as `go/ast` reads it - consecutive comment lines with no blank
line or code between them, blank `//` separator lines included, and a `/* */`
comment counting every line it spans. In the others it is a run of consecutive
`#` lines; a `#!` shebang on the first line is not a comment. Directive lines
are read by tools rather than people and do not count: `//go:...`,
`//nolint:...` and any other `//name:` directive, and gosec's `// #nosec`. The
blank `//` line gofmt puts between a doc comment and a directive that ends it
does count, so a doc comment followed by `//nolint` has room for two lines of
text.

The limit applies to package doc comments and to tests as much as to anything
else. Reasoning that needs more than three lines belongs here in `docs/`: how a
package works in [How it works](architecture.md), the boundary it enforces in
[Security](security.md). Splitting one long comment into several short blocks
passes the gate and defeats it; don't.

### `internal/lockaudit` - the holder context

Every command that takes the cache backend's exclusive lock must run its work
under the holder context that lock returned, and must judge the outcome against
that same context. The gate parses the source and requires, per command: exactly
one lifecycle initialization, a holder context bound to a name rather than
discarded into `_`, exactly one call to the work function, that context as the
work call's first argument, the work call sitting inside the lock-loss verdict,
and every return after initialization being either a bare `nil` or that verdict.

A second table requires each collection command to go through the shared funnel
rather than taking a backend lifecycle of its own.

Both tables are closed: **adding or renaming such a command means editing them**.
That is the gate's one acknowledged gap - nothing detects a third lifecycle
function, or a fourth collection command, that was never added. It is
deliberately not a general context-threading linter.

It reads source because the seam cannot be driven at runtime: the lifecycle
functions construct the backend themselves, so no test can inject one able to
lose its lock, and the local backend's `Lock` hands back the caller's own
context and never loses one. The gate therefore proves the calls are composed,
not that the composition behaves; `LockLostError`'s own decision table is
pinned by its tests in `internal/galaxy/cache`.

### `internal/ciaudit` - the linter version

One value is spelled twice. The golangci-lint release lives in
`.github/workflows/ci.yml` as the action step's `version` input and in the
`Justfile` as `GOLANGCI_LINT_VERSION`. Nothing resolves those against each
other, so a bump that edits one and forgets the other is silent - and under
`default: all`, a release difference is a findings difference. CI's spelling is
the one held to an exact `vMAJOR.MINOR.PATCH` - `latest` and a truncated `vX.Y`
are both refused, because neither can disagree with anything while still
changing what CI enforces - and the Justfile's is then held to equal it.
**Bump both spellings in one commit.**

The workflows themselves are checked by `go tool actionlint`, not here. This
package used to resolve a job's `needs` and a local `uses:` by hand, written
after a release workflow was rejected at dispatch and published nothing at all.
actionlint resolves both, plus the inputs and secrets a reusable-workflow call
passes, plus expression syntax, runner labels and action inputs - everything the
hand-written version had ruled out of scope as belonging to a real linter. The
version pin stays here because it is the one thing actionlint cannot know: it
reads an input a second file duplicates, never the `@v9` the action itself is
pinned at, which nothing duplicates.

### `internal/galaxy/store` - the dirty flag

Every method on the snapshot value that takes the write lock must also set the
dirty flag, so a run that changed nothing can skip its save. The gate parses the
snapshot source; read-locked methods are out of scope. There is one exemption, a
literal name rather than a pattern: unmarshalling is a load, not a write, and
setting the flag there would make every S3-backed run dirty on arrival.

Its scope is one file, which is stated rather than implied: a mutator added
elsewhere is covered only by a hand-maintained table in the same package, so a
new mutator needs a row there too.

`Dirty` means "this process called a mutator since the store was loaded or
built", never "the store differs from what the backend holds": every mutator
sets it unconditionally, even when the value did not change, and nothing clears
it, because a false positive costs one redundant save while a false negative
silently drops state.
`cache.WithCleanSaveSkip` is what skips the save of a clean store, which is also
why a run that saves nothing never applies the persist-time eviction and
redaction.

A new bucket touches several places that only tests tie together: the `Store`
field with its json tag; `New` and `ensureMaps`, because an explicit JSON
`null` in an S3 snapshot nils a map, and a write into a nil map panics in a
worker goroutine and kills the process with the S3 lock still held, stalling
every other run on the bucket until the lock's ten-minute TTL; `snapshotData`,
its copy and `MarshalSnapshot`'s field-by-field assignment; a
`helpers.StoreBucket*` name and a `jsonBuckets` entry, appended rather than
inserted, since the save-rollback test depends on that order; the schema
version bump; and the dirty flag in each mutator. A bucket recording content on
disk is counted by `hasContentEntries` and survives `ClearCaches`; one holding
an answer from a remote is reset by it.

### `internal/galaxy/archive` - the probe decompressor

The tar.gz shape probe must reach its decompressor through the bounded
constructor and through nothing else, and must spell both sizing arguments as
the named constants rather than as inline literals - a call handed literals is a
call those constants no longer govern. One companion test bounds both constants,
so what is gated is the byte budget rather than a constructor's name: the block
size must stay above 512 bytes, because pgzip silently turns 512 or less into
its 1 MiB default, and the reservation must stay within 256 KiB, which the
extractor's four 1 MiB blocks would fail. The gate reads source because the
reservation cannot be observed from outside.

The probe's decompressed budget, `helpers.ArchiveProbeMaxBytes`, has a floor of
4,196,352 bytes: the most `archive/tar` can be made to read before `Next`
returns its first header - a 512-byte block plus a body of up to 1 MiB for each
of the `x`, `L` and `K` meta headers, then the returned header's block and a
sparse map of up to 1 MiB. A budget under that floor refuses archives
`archive/tar` accepts, with `ErrArtifactTarHeaderNotFound`, which the download
path treats as terminal. `TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling`
holds the constant between that floor and 16 MiB, and
`TestMetaHeaderCeilingIsWhatArchiveTarReads` builds the composite and asserts
`archive/tar` reads exactly that much. A toolchain whose `archive/tar` reads a
different amount fails only the second test; re-derive the figure then and
update it in both tests and in the constant's comment. The 5 MiB cap stops
short of 5,245,440 bytes, where a fourth meta body would end; that body carries
nothing new, since a repeated meta kind replaces the earlier one, and is refused
on purpose. A floor grown by one more term lands exactly on that figure, and a
budget set to meet it admits the redundant body too - so re-derive the floor
rather than rounding the budget up.

### `internal/gzipstream` - the pgzip monopoly

No non-test file outside `internal/gzipstream` may reach a pgzip member other
than its writer API - test files, `vendor/` and dot-directories are out of scope
deliberately. Resolution is by import path rather than by the identifier `pgzip`,
so no alias can hide the next one - the defect that prompted the gate was an
import aliased to `gzip`, which read like the standard library and which no
search for the word could find.

It is an allow-list of writer members rather than a blocklist of reader
constructors, because a blocklist was measured missing two evasions: a
constructor held as a function value, and a zero value turned into a reader by
`Reset`. Open gzip readers through `internal/gzipstream` instead.

### `internal/galaxy/projectfile` - the toml monopoly

No non-test file outside `internal/galaxy/projectfile` may import
`github.com/BurntSushi/toml`; test files, `vendor/` and dot-directories are out
of scope, as for pgzip. Resolution is by import path, so an alias, a dot import
or a blank import cannot hide a second importer, and the gate first checks that
it sees the package's own decoder importing the library, so a rename cannot
leave it passing over nothing. One importer keeps one place rendering a TOML
parse error, and that rendering is deliberately position-only - line and last
key, never the library's message, which echoes the file's own tokens (see
[Security](security.md#loading-requirementsyml-and-the-lockfile)).

### `internal/cache/s3` - a class row for every sentinel

Every package-level variable in the S3 backend's non-test files named like a
sentinel - `err` or `Err`, then an upper-case letter - needs a row in
`sentinelClassCases`, which pins it to the unavailable, unusable or busy class,
or to none, and checks it matches that class and neither other one: a
sentinel carrying two classes would exit by the order `exitcode.FromError`
checks them in, and a reclassified one moves an exit code. The gate reads the
declarations from source, so a sentinel added without a row fails rather than
going unchecked, wherever in the package it is declared. It reads the table
from source as well, since only the source says which variable a row passes,
and fails a row whose name is not that variable's, a variable listed twice and
a row for anything the package does not declare - and it fails when it finds
no sentinel at all, rather than passing an empty enumeration.

## Lint

`.golangci.yml` runs with `default: all` and a short disable list, so expect
linters most projects leave off. Two settings matter beyond that.

**depguard** carries an explicit import allow-list. Anything outside it is a
lint error on import: the standard library, `go/ast`, `go/parser` and `go/token`
(which the audit packages need and which the standard-library expansion does not
cover), this module, and the eleven direct dependencies -
`Masterminds/semver/v3`, `ProtonMail/go-crypto`, `go-git/go-git/v5`,
`go-git/go-billy/v5`, the `ssh` subtree of `golang.org/x/crypto` (the entry
is a prefix: `ssh`, `ssh/knownhosts` and `ssh/agent` pass, the rest of the
module does not), `klauspost/pgzip`, `psvmcc/hub`, `urfave/cli/v3`,
`go.etcd.io/bbolt`, `go.yaml.in/yaml/v3` and `github.com/BurntSushi/toml`,
which is imported by exactly `internal/galaxy/projectfile` (the
[toml monopoly gate](#internalgalaxyprojectfile---the-toml-monopoly) keeps it
so).

**No test-file exclusions.** Every linter applies to `_test.go` too.

`fieldalignment` runs in both `just check` and CI, so struct field order is not
a style choice: a layout that wastes memory through padding fails the build,
test-only structs included. Run `just fix` to reorder in place, then re-run
`just check`. For the same reason, do not respell a dependency's anonymous
struct type as a local literal to build values assignable to it: `just fix`
may reorder the local copy but never the dependency's declaration, and the
assignment stops compiling. Append a zero element whose type is inferred from
the target slice instead, as fakegalaxy's `appendRow` does.

Almost every `//nolint` in this codebase carries a reason after it, and the
handful that do not are worth fixing rather than copying. Nothing enforces it:
`nolintlint` is on under `default: all`, but its explanation requirement is not
configured.

## Test conventions

Tests sit beside the code, in the same package by default. The exceptions are
the `internal/galaxy/collections` end-to-end suite and a few files in
`internal/galaxy/cache` and `internal/galaxy/fetch`, which are
`package <pkg>_test` so they drive the public API from outside; the
`testpackage` linter is disabled so both styles are legal. In
`internal/galaxy/cache` it is also the only way to use a real backend, since
`internal/cache/local` imports that package and an in-package test importing it
back would be a cycle.

A test's comment states the property being pinned, in at most three lines.
Positive controls are treated as mandatory rather than optional - a detector
that finds nothing passes a monopoly gate perfectly.

Expected values are spelled as literals rather than derived from the production
constant they check - the archive boundary sizes and probe ceiling, the manifest
chain's document names and caps, the signature framing's octets and messages -
because an expectation computed from the constant under test moves with it and
can never fail. Changing such a constant therefore fails the suite until the
literals are updated deliberately; do not refactor them into references.

Some fixtures pass vacuously in ways worth knowing before writing one:

- **Offline.** For Galaxy API traffic, `--offline` is enforced by the
  network-refusing client `fetch.NewOffline`, which the command wiring selects
  when `cfg.Offline` is set. A test proving an offline path touches no network
  runs over that client and starts from cleared metadata caches
  (`Store.ClearCaches`); one that sets `cfg.Offline` over a live fakegalaxy
  client, or with a warm API cache, passes with the offline check broken.
- **Secrets in JSON.** `encoding/json` escapes `&`, `<` and `>`, so a needle
  spanning the `&` between two query parameters never matches serialized
  bytes, even when the secret leaked. Search for a single parameter
  (`X-Amz-Signature=...`) and pair the absence with a positive control.
- **Hand-assembled tar.** A fixture built from raw 512-byte blocks, for the
  entry kinds `tar.Writer` refuses to encode, must end with the two zero
  trailer blocks; without them a short final read produces the error on its
  own, and a refusal test passes without the rule it tests.
- **Config.** A test that builds a `Config` hides `./ansible.cfg` and
  `~/.ansible.cfg` (`neutralizeAnsibleDiscovery`), but
  `/etc/ansible/ansible.cfg` stays reachable on the machine running the
  suite. Assert only values an explicit flag or environment source set, which
  outrank the file; make them differ from the flag defaults, so a row cannot
  pass with no source read; and keep `:` out of a path value, since config
  splits collections and roles paths as a POSIX search list and keeps only the
  first entry.

Two kinds of test must never call `t.Parallel`: those in the collections suite
that call `captureStdIO`, which swaps the process-wide `os.Stdout` and
`os.Stderr` because `progress.New` reads them at construction and has no other
output seam; and those in `internal/galaxy/signature` that measure allocation
through `runtime.MemStats.TotalAlloc` or wall-clock time, both process-wide.

The solver's tests gate completeness as well as soundness.
`TestOracleMembership` brute-forces every assignment of small generated graphs
and fails when `Solve` rejects a graph the oracle can solve, accepts one it
cannot, or returns anything but a closed minimal resolution; `FuzzSolve`
applies the same checks to graphs under `fuzzOracleCap`. The oracle judges
membership through Masterminds' `Check`, never through the solver's own set
algebra, so the two stay independent. Lowering `oracleSeedCount` weakens the
gate.

**Nothing in the suite dials a real host.** Galaxy API paths go through
`internal/testing/fakegalaxy`, an in-memory Galaxy v3 API double that also
serves the v1 role API, while the S3, fetch and signature packages stand up
`httptest` servers of their own. The double
answers the same routes and JSON shapes the real API does, generates
deterministic `tar.gz` artifacts with matching digests - fixed modes and a fixed
modification time, never the clock - and offers:

- **content**: add a version with its dependencies and get back the digest, so
  no test hardcodes a checksum; render its manifest; sign it.
- **fault injection**, per endpoint and per collection: a status code, a hang
  until the request context ends, a stall after N bytes, or a byte-drip. They
  exercise different deadlines and are not interchangeable. A stall (artifact
  and tarball routes only) flushes a real prefix and then blocks - the shape a
  read-inactivity watchdog must catch. A drip never stops making progress, so
  it defeats that watchdog and only a whole-transfer or metadata deadline ends
  it: on the download routes it writes the real bytes one per interval,
  cycling forever, and on a JSON route it answers `{` and then one space per
  interval, a document that never parses. A well-formed fault sets exactly one
  of those. A `Count` of zero never fires, a positive one is spent per
  matching request, a negative one fires forever. A test arming a hang, a
  stall or a drip must abort the request itself, or shutdown blocks.
- **request counting** per endpoint, which is how "this path makes no metadata
  request" is asserted rather than assumed. Every route counts first, checks
  auth second and enacts an armed fault third, so a request that an auth
  failure, a fault or a 404 ended is still counted, and a fault can never mask
  a missing or wrong header. A route added to the double keeps that order.
- **auth**: require a value, choose the failure status, and capture the header
  actually received.
- **roles**: `AddRole` registers `owner.name` as imported from a GitHub user
  and repository with a default branch and a version list (tag names with an
  optional `commit_sha`, listed in registration order, oldest first as
  galaxy.ansible.com lists them), after which the double answers the v1
  routes - `roles/?owner__username=&name=` and the paginated
  `roles/<id>/versions/` with `next_link` - under the v1 root matching the
  shape the double was mounted with (`/api/v1` for galaxy.ansible.com's, `/v1`
  for a Galaxy NG base path). Before the first `AddRole` it answers 404 for
  them, which is how a server without a role API (an Automation Hub) is
  modeled.

The constructor fixes the deployment shape. `New` is galaxy.ansible.com, with
collections under `/api/v3`; `NewAtBasePath` is a Galaxy NG or Automation Hub,
with `v3` directly under the base path (an empty one is a hub at the root) and
`/api/v3` answering 404, so a client's API-root probing meets what a real hub
answers. Every URL the double generates for itself carries the base path.

Both constructors take a `testing.TB`, deliberately: that is the one interface
implementable only by the standard testing package, so the double can never be
reached from production code. The one exported entry without it is
`BuildArtifact`, which touches no server state and returns the bytes and digest
`AddVersion` would serve, for doubles that fabricate an artifact themselves.

A double never takes its wire shape from the code it tests. fakegalaxy spells
`MANIFEST.json`, `FILES.json`, the `sha256` checksum type and the `signature`
key as local literals: a shared constant would change on both sides at once,
and a defect in the manifest reader could cancel a matching one in the
generator. Its artifact is a closed chain `manifest.VerifyChain` accepts end to
end, so a signature over `ManifestJSON` verifies against the bytes served.

Git paths go through `internal/testing/fakegit`, its sibling for a git remote:
an in-process smart-HTTP server over `httptest` and an ssh listener over
`golang.org/x/crypto/ssh`, both serving repositories built in memory with a
fixed committer time (so every commit hash in a test is reproducible), with
capability toggles (`shallow`, `allow-reachable-sha1-in-want`), the same fault
grammar (status, hang, stall, plus a pack built from another commit and a
redirect to another server), request counting per endpoint and auth capture
(the Basic header over http, the key fingerprint over ssh). It also hands out
generated client keys, an in-process ssh-agent on a unix socket, and a
known_hosts file for the listener, and its `AddRole` stages a minimal role
tree at a repository root (`meta/main.yml` with the dependencies given,
`tasks/main.yml`, `defaults/main.yml`) beside `AddCollection`. Its own tests drive the stock go-git client
against every shape, which is what keeps the wire framing honest. A test that
uses the ssh half sets `SSH_KNOWN_HOSTS`, `SSH_AUTH_SOCK` or `HOME` through
`t.Setenv` and therefore runs serially; the agent socket lives in a short
`os.MkdirTemp` directory rather than under `t.TempDir`, because darwin caps a
unix socket path at 104 bytes.

fakegit owns only the server side and validates nothing a repository carries:
`RawTreeCommit` encodes `..`, `.git`, `a/b`, duplicates and submodules, and
`Symlink` records any target verbatim, because refusing a hostile shape is the
code under test's job - which is also why its pack walk is hand-written rather
than go-git's, whose tree walker refuses those entries. The trees `AddCollection`
and `AddRole` write are literals, not imports from the builders. Unlike
fakegalaxy, it counts a request and captures its credential, then lets an armed
fault answer, and only then applies `RequireAuth`, so a fault is observable even
on a request that would fail auth. Faults win in the order redirect, status,
hang, stall; over ssh only a hang, a stall and `ServeCommit` apply. In the
zero-value `Capabilities` a deepen is a bad request and a want that is not an
advertised tip gets an `ERR` line, never a silently served full pack. A test
arming a hang or a stall must end the request itself, as with fakegalaxy.

The collections suite drives its pipeline through an in-memory
`gitsource.Client` double with no transport at all
(`git_fake_client_test.go`, with `git_fake_roles_test.go` supplying
`AcquireRole` over role trees built from `internal/testing/faketree`), and
leaves the transport to `internal/galaxy/gitfetch`'s own tests against fakegit,
`role_test.go` among them for the role half. The role pipeline's end-to-end
coverage is `roles_e2e_test.go` in that suite: Galaxy and git roles through
install, warm, lock, `--frozen`, `--offline`, `--refresh`, `--no-cache`,
`--dry-run`, the dependency walk, the directory policy and cleanup, against
fakegalaxy's v1 routes and the client double.

`internal/testing/faketree` is the in-memory `treearchive.Source` the builder
packages and the client double build from: files, executables, directories,
symlinks and submodule entries with a fixed commit time, plus a declared-size
override for the budget tests. `internal/galaxy/treearchive`,
`internal/galaxy/collectionbuild`, `internal/galaxy/rolebuild` and
`internal/galaxy/galaxyv1` each carry their own tests beside the code; none of
the source audits changed for roles.

**The S3 lock tests wait on events, never on elapsed time.** `testLockTiming`
shrinks the lock's own intervals, but the client's retry policy cannot be
shrunk, and one jittered backoff can outlast a heartbeat, so a test waits on
something observable - `waitForLockEvent` over the fake's request count, or the
holder context closing - and every wait ceiling is a liveness bound, so a slow
machine only slows a test down. A fault meant to reach a caller's error branch
is armed with `failNext` for `s3RetryMaxAttempts` requests or indefinitely
(`-1`); a smaller one is absorbed by the retry, and the test passes with the
branch untested. Race tests force their interleavings rather than sample them:
the interference runs inside the handler after the fake has written its
response, and the fake never flushes a HEAD response, so the client sees it
strictly afterwards; the one statistical race test carries a positive control,
so a machine whose timing falls outside its sweep fails instead of passing. A
transport that strips cancellation (`answeredDespiteCancelTransport`) belongs
only on a fixture that answers every request: on one that hangs a request on
purpose, the caller's cancellation is the only thing that ends it. `fakeS3`
never verifies SigV4, so signing correctness rests on
`TestRequestURLSignedPathMatchesSentPath` and `TestAwsURIEncodeMatchesS3`. It
answers a listing in one page unless a test sets a page size with
`setListPageSize`; each truncated page then carries a fresh opaque continuation
token, and a token the fake never issued is answered `400`, so a client that
resumes any other way than by sending the token back fails.
`setListOmitNextToken` leaves the token out of a truncated page, a reply both
listing loops must stop at rather than list again from the first page.

There is exactly one `testdata` directory, under `internal/galaxy/signature`,
holding keyrings and a family of detached signatures covering the valid,
expired, revoked, outsider and malformed cases. It is committed gpg 2.5.21
output, never generated at test time (key generation is slow and needs
gpg-agent), made in throwaway `GNUPGHOME`s with `--batch`, loopback pinentry
and an empty passphrase. It holds public material only, with one exception:
`secret.gpg`, `secret.asc` and `secret-public.asc` are a discardable key that
signs nothing, is trusted by nothing, and exists only to prove `LoadKeyring`
refuses secret key material.

Every key is `gpg --quick-generate-key '<uid>' ed25519 sign never`, except the
expired one (`sign seconds=10`). `public.asc` and `second.asc` are exports of
two separate keys, and `two-keys.asc` is the two concatenated, which relies on
gpg's trailing newline. The signatures are `gpg -u <key> --detach-sign
[--armor]` over `manifest-a.json` or `manifest-b.json`, which differ only in
their declared version; `sig-a-badarmor.asc` and `sig-a-badbase64.asc` are
edited copies of `sig-a-valid.asc` rather than signatures. A regenerated
set must keep what the tests read from the bytes: the expired key signs and is
exported inside its ten-second window, and `sig-a-expsig.asc` carries a
five-second signature expiry, so `EXPKEYSIG` and `EXPSIG` do not depend on the
test machine's clock; the revoked key has its revocation certificate imported
before export; `keyring.asc` holds the signer, second signer, expired and
revoked keys but not the outsider, which is what produces `NO_PUBKEY`; and
`signing-subkey.asc`, a key given a signing subkey by `gpg --quick-add-key`, is
the only material carrying an embedded primary-key-binding signature. Shapes
gpg does not write are spelled in `verify_test.go`'s `synthesizedBlobs` instead
of committed.

Adding a packet-bearing fixture also means listing it in `framing_test.go`'s
`gatedFixtures` and restating `committedPacketMeasurements` there, plus
`largestFixtureHeaderSection` when the fixture carries armor header lines.
The fuzz target caps allocation at a floor plus a per-byte ratio of the input;
if a legitimate input ever exceeds it, raise the constant and record the
measurement, never add a special case for the input.

## The benchmark harness

### testing/bench.sh

`testing/bench.sh` measures `ansible-galaxy` against `go-galaxy` across
`requirements-{1,10,100}.yml` in six scenarios: `cold`, `warm`, `frozen`
(go-galaxy only) and their three S3 equivalents, and across
`requirements-roles.yml` (ten Galaxy roles with no dependencies between them)
in two more: `roles-cold` and `roles-warm`, `ansible-galaxy role install`
against `go-galaxy install --roles-path`. Every measured command passes
`--no-deps`, so what is compared is fetch plus extract.

```bash
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o ./dist/go-galaxy ./cmd/go-galaxy
docker compose -f testing/docker-compose.yaml up -d minio-svc   # for the s3-* scenarios
testing/bench.sh
```

It needs `hyperfine` and `python3` on `PATH`, the built binary, and the
virtualenv. The binary and the virtualenv are each reported with the command
that supplies them; `hyperfine` and `python3` are reported by name only. The S3
scenarios are dropped with a warning, rather than failing the run,
when the endpoint does not answer, so the local scenarios still run on a machine
with no container runtime.

Knobs and defaults: `RUNS=5`, `WARMUP=1`, `SIZES="1 10 100"`,
`SCENARIOS="cold warm frozen s3-cold s3-warm s3-frozen roles-cold roles-warm"`,
`S3_ENDPOINT=http://127.0.0.1:9000`, plus the bucket and credentials.

Every cache it wipes lives under `$TMPDIR` - never in `$HOME`, because a
benchmark that wipes the caches it measures must not wipe the ones you use, and
never inside the repository, because extracted third-party collections are Go
source often enough that a tree-walking linter picks them up as this module's.

Output lands in `dist/bench/`: `<scenario>-<N>.md` per scenario and size,
`roles-cold.md` and `roles-warm.md` for the role scenarios,
`resources-<N>.md` with peak RSS and bytes downloaded from a separate single-run
pass, and `summary.md` concatenating everything with the tool versions and host
recorded. See [Benchmarks](benchmarks.md) for the published numbers.

### go-galaxy-benchmark

`cmd/go-galaxy-benchmark` is the same comparison as a Go binary, narrowed to
collections in `cold` and `warm` with no S3. What it measures and what its
output looks like is written up once, in
[Benchmarks](benchmarks.md#go-galaxy-benchmark); what follows is how to run it
from a checkout and what a run does with its environment and its failures.

```bash
just build
dist/go-galaxy-benchmark run \
  --ansible-galaxy .venv/bin/ansible-galaxy \
  --go-galaxy dist/go-galaxy \
  --work-dir /var/tmp/gg-bench \
  --requirements-dir testing \
  --sizes 1,10,100 --runs 5
dist/go-galaxy-benchmark show --report /var/tmp/gg-bench/report.json --format svg --out bench.svg
```

`--work-dir` should be on real disk. The workload is mostly inode creation, and
a run on tmpfs describes no storage anyone deploys on.

The harness overrides only the variables it sets itself - `GO_GALAXY_CACHE_DIR`
and `TMPDIR` for go-galaxy - so any other `GO_GALAXY_*` in your shell still
reaches the measured binary: an exported `GO_GALAXY_S3_BUCKET` turns the run
into an S3 run. Every measured command gets a closed stdin, so a tool that
prompts exits on EOF instead of looking hung. A failing run is counted in the
report's `failed` field, with its `last_error`, and left out of `samples_ms`
rather than ending the series; an interrupt ends the whole measurement and
writes no report.

## Dependencies

`vendor/` is **git-ignored, not committed**. It is regenerated locally by
`just deps` and is absent in CI entirely, so a local build resolves through it
while CI downloads modules. The practical consequence is local: Go selects
vendor mode automatically when the directory exists and refuses to build when
`vendor/modules.txt` disagrees with `go.mod`.

Adding a dependency is therefore two steps, and skipping either fails in a
different place:

1. add the module path to the depguard `allow:` list in `.golangci.yml`, or
   `just lint` and CI reject the import (the list names go-git, go-billy and
   `golang.org/x/crypto/ssh` for the git transport and its test double, and
   those three are imported by exactly `internal/galaxy/gitfetch` and
   `internal/testing/fakegit`; `BurntSushi/toml` for galaxy.toml, imported by
   exactly `internal/galaxy/projectfile`, which its gate enforces);
2. run `just deps`, or Go's own vendor-consistency check fails the build and
   names what is out of sync.

`just check` deliberately does not depend on `just deps`: a gate that rewrites
`go.mod`, `go.sum` and `vendor/` before reading them can only ever agree with
itself.

`govulncheck` runs in both `just check` and CI, so a vulnerable dependency fails
the build. The release configuration runs no `before` hooks at all, and `go mod
tidy` is the one whose absence is deliberate rather than incidental: a release
must build the dependency set that was committed and reviewed, and tidy would
rewrite it in the one build that gets published.

**A Go toolchain bump can fail tests with no code change.** `compress/flate`
output is outside Go's compatibility promise, so `collectionbuild`'s
`TestBuildGoldenDigest`, the one test pinning the gzip header and the lead
documents' encoding, can move on its own. If `TestBuildStructure` and
`rolebuild`'s `TestBuildArtifactShape` still pass, only the deflate bytes moved
and the new digest is the fix; if they fail too, the writer changed the
artifact's shape. A change in what `archive/tar` reads before its first header
fails the [probe ceiling test](#internalgalaxyarchive---the-probe-decompressor)
instead.

**The solver mirrors Masterminds/semver.** `internal/galaxy/solver`'s
`versetbuild.go` replicates the pinned release's constraint grammar - its
regexes, range rewriting and each comparator's branch structure, quirks
included - while
`semver.NewConstraint` stays the sole accept/reject authority. After a semver
bump, `TestVerSetDifferentialAgainstCheck` and `TestVerSetGroundTruthRows` hold
set membership to bug-for-bug agreement with `Check`; a divergence is a builder
bug to fix in `versetbuild.go`, never a reason to weaken the test's authority.

**The signature framing gate rests on go-crypto behavior**, which
`framing_test.go` measures rather than assumes: every packet is read to exactly
the end the gate computes, even on a failed parse; its length encodings agree
with go-crypto's header reader; an MPI stays sized by its two-octet bit length;
and allocation stays linear in the packet. After a go-crypto bump, a failure
there means a premise of the gate changed - investigate it before moving any
ceiling constant. v5 signatures and v5 secret keys are refused before a length
is read only because go-crypto's `!v5` build constraint sets
`packet.V5Disabled`, so
go-galaxy must never be built with `-tags v5` (`TestV5ParsingStaysDisabled`).

## Sentinel errors and exit codes

`cmd/go-galaxy/exitcode` picks the [exit code](exit-codes.md) by matching
sentinels with `errors.Is`, class by class, and checks cancellation before
anything else. Which sentinel a producer wraps with `%w` therefore decides the
class.

A sentinel naming a condition this program or a remote drove - a stalled read,
the artifact, metadata, state-object and signature-fetch deadlines, a lost cache
lock - renders its context cause with `%v`, never `%w`. That cause is usually
`context.Canceled`, since the read watchdog cancels its own context to unblock a
read and a lost lock cancels the holder context, and wrapping it would report a
stalled server or a stolen lock as a Ctrl-C. `ErrSignatureSourceUnavailable`,
which wraps its transport cause with `%w`, is the one deliberate exception. A
deadline sentinel is applied only when the parent context is live and the
operation's own budget expired; the metadata and state-object deadlines also
require the error to carry a context error, so an HTTP status error that raced
the budget keeps its identity.

Several reclassifications are deliberate. `ErrEmptyGzipMember` belongs to no
predicate: each reader wraps it into its own class - the shape probe's
`ErrArtifactNotTarGz`, the install aggregation's exit 5, the S3 backend's
`ErrCorruptStateObject` - and claiming it in the artifact-shape predicate would
report a corrupt S3 snapshot as an install failure on a run that installed
nothing. The S3 backend renders `ErrResponseTooLarge` with `%v` when it reports
an oversized state object, which would otherwise match exit 4 and exit 9 at
once, and `ErrGitArtifactSelfCheck` carries its cause as text so a builder
defect is not reported as the remote's integrity failure.

The exit-code tests are closed tables, and nothing enumerates the sentinels in
`helpers`: a new sentinel needs both a predicate and a row in the matching
table, or it silently exits 1 - `ErrInvalidRequirementsTOML`, for instance,
joined `isInputFileUsageError` and its test table in one change. Those
deliberately left at 1 are listed in
`genericSentinels` with their reason. The sentinels `internal/galaxy/extracted`
exports are not classified by name either, because every path that raises one
runs inside an install or warm worker, whose failures arrive behind
`ErrInstallationFailed`; one raised outside a worker would exit 1. The tables
build the `%v`-rendered shapes themselves, so a producer switching to `%w` is
caught only by the producer's own tests
(`TestMixedDripAndStallDoesNotClassifyAsInterrupt`, `TestLockLostError`).

The index `go-galaxy --help` prints uses the leading phrase of each class's row
in [Exit codes](exit-codes.md). Its test derives the codes from the `exitcode`
constants, so a renumbering fails it, but the phrases are literals nothing
compares with the document: changing a row's leading phrase means editing the
root command's description and that test by hand.

## Conventions

- A comment is at most three lines, package doc comments included; the
  reasoning behind a package lives in [How it works](architecture.md) and
  [Security](security.md). Keep both true after a change.
- Comments state constraints and reasons, not narration.
- Only hyphen-minus, everywhere, enforced by test.
- `README.md` is an index; reference material lives in `docs/`. A behavior
  change lands with the document that describes it.

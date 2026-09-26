# Development

Everything here runs from a checkout with Go, `git` and
[just](https://github.com/casey/just); `just lint` also needs golangci-lint at
the pinned release. The test suite needs no network, container runtime or
Python: only the benchmark harness and the documentation site do.

## Running the tests

| Recipe | Runs |
| --- | --- |
| `just test` | `go test ./...` |
| `just check` | `go vet`, staticcheck, govulncheck, fieldalignment, actionlint |
| `just lint` | golangci-lint, refusing any release but `GOLANGCI_LINT_VERSION` |
| `just fix` | `go fix` and `fieldalignment -fix` |
| `just deps` | `go mod tidy && go mod vendor`, after any dependency change |
| `just build` | check, lint and test, then `dist/go-galaxy` and `dist/go-galaxy-benchmark` |
| `just docs`, `just docs_build` | the [documentation site](#the-documentation-site) |

```bash
# what CI runs
go vet ./... && go tool staticcheck ./... && go tool govulncheck ./... && go tool fieldalignment ./...
go tool actionlint -shellcheck= -pyflakes=
go test -v -race -coverprofile=coverage.txt ./...

# one package, one test, benchmarks, a fuzz target past its seeds
go test ./internal/galaxy/archive/ -run 'TestProbeTarGzUsesTheProbeSizedDecompressor'
go test ./internal/galaxy/solver -bench . -run '^$'
go test ./internal/galaxy/solver -fuzz FuzzSolve -fuzztime 60s
```

- Run with `-race` before calling concurrency work done.
- The `go tool` binaries come from the `tool` directive in `go.mod`.
- `go.yaml.in/yaml/v4` stays at `v4.0.0-rc.3`, which actionlint v1.7.12
  compiles against; `go get -u tool` can move it past that.
- actionlint's shellcheck and pyflakes stay off in both spellings, so no check
  fires only on CI's runner.
- The four fuzz targets run only their seed corpora under `go test`.

> [!WARNING]
> Do not run the suite as root: tests asserting a permission refusal skip
> there, and the coverage vanishes silently.

## CI

| Workflow | Trigger | Runs |
| --- | --- | --- |
| `ci.yml` | push to `main` (not `**.md` alone), pull request, `workflow_call` | the check commands, pinned golangci-lint, the `-race` suite, a non-failing coverage upload |
| `docs.yml` | push to `main` touching `**.md` | `go test ./internal/proseaudit/` |
| `release.yml` | a `v*` tag | `ci.yml` first, then GoReleaser, attestation, the major tag move |
| `action.yml` | a change to `action.yml` or itself; manual | the composite action against galaxy.ansible.com |

- `.goreleaser.yml` runs no `before` hooks: the gate job ran the suite, and
  `go mod tidy` would rewrite the reviewed dependency set.
- A tag with `-` is a prerelease: no Homebrew cask, no `latest` image, no
  major tag move.
- The major tag (`v1`) is the one tag that does not fix a state, hence
  `--match` in the Justfile's `git describe`.

Release notes come from [commit subjects](https://github.com/greeddj/go-galaxy/blob/main/CONTRIBUTING.md#commit-subjects);
checking a release is [Verifying a release](../guides/security.md#verifying-a-release).

## The repository audits itself

Every gate is an ordinary test: three test-only packages plus five audit
files inside the package they gate. A gate fails when a function or file it
names is gone, so a rename cannot switch it off; only the dash and
comment-length gates skip when `git` cannot list the tree.

| Gate | Enforces | To satisfy it |
| --- | --- | --- |
| `proseaudit`: dashes | no U+2014 or U+2013 in a tracked file | type `-`; a test spells one as bytes or `\uXXXX` |
| `proseaudit`: line citations | `foo_test.go:NNN` only on a failure-reportable line; no production line | renumber after inserting lines, or cite an identifier |
| `proseaudit`: comment length | no comment block over three lines in Go, `.sh`, YAML, `Justfile`, `Dockerfile`, `.gitignore` | move reasoning to `docs/`; never split one comment into blocks |
| `lockaudit` | a lock-taking command works under the holder context, judged by `LockLostError` | add a new or renamed command to `holderCases` or `delegateCases` |
| `ciaudit` | CI's golangci-lint `version` is exact and equals `GOLANGCI_LINT_VERSION` | bump both in one commit |
| `store`: dirty flag | every write-locked `*Store` method in `snapshot.go` sets `dirty`, bar `UnmarshalJSON` | a mutator elsewhere needs a `TestEveryMutatorMarksDirty` row |
| `archive`: probe decompressor | `ProbeTarGz` calls only `gzipstream.NewReaderN(..., probeGzipBlockSize, probeGzipBlocks)` | keep the constants: block above 512 bytes, reservation within 256 KiB |
| `gzipstream`: pgzip monopoly | no non-test file elsewhere uses pgzip beyond its writer API, by import path | open gzip readers through `gzipstream` |
| `projectfile`: toml monopoly | no non-test file elsewhere imports `BurntSushi/toml` | decode through `projectfile` |
| `cache/s3`: sentinel classes | every package-level `err`/`Err` sentinel has a `sentinelClassCases` row | add it: unavailable, unusable, busy or none |

<details markdown>
<summary>Exact rules: citations, comment blocks, lockaudit, the probe budget</summary>

**Citations.** A reportable line is a `Fatal`, `Error`, `Log` or `Skip` call
on a testing value (any line of it), a call to a same-package `t.Helper()`
helper, or a declaration line. The cited file resolves by base name in the
citing directory. This gate walks the filesystem, skipping `vendor/`,
`testdata/` and dot-directories, so untracked `.go` files count and must parse.
A `just fix` reorder can shift cited lines too.

**Comment blocks.** In Go a block is a `go/ast` comment group: blank `//`
lines count, and a `/* */` counts every line it spans. Directive lines
(`//go:`, `//nolint:`, any `//name:`, `// #nosec`) do not count, but gofmt's
blank `//` before one does, leaving a doc comment two lines of text.

**lockaudit.** Per command: one lifecycle initialization, a named holder
context, one work call taking it first, inside the lock-loss verdict, and
every later return a bare `nil` or that verdict. It proves composition, not
behavior; `LockLostError` has its own tests.

**Probe budget.** `helpers.ArchiveProbeMaxBytes` (5 MiB) sits above
4,196,352 bytes, the most `archive/tar` reads before its first header.
`TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling` bounds it and
`TestMetaHeaderCeilingIsWhatArchiveTarReads` measures it; re-derive the figure
if the second fails.

</details>

The rest of a new snapshot bucket (`ensureMaps`, `jsonBuckets`, the schema
version) is in [Cache and storage](cache.md). A new sentinel's exit-code rules
are in [HTTP, output and exit codes](http-output-exit-codes.md).

## Lint

`.golangci.yml` runs `default: all` with a short disable list and no test-file
exclusions, so expect linters most projects leave off, in tests too.

- depguard allows only the standard library, `go/ast`, `go/parser`,
  `go/token`, this module and the direct dependencies; any other import fails.
- Three monopolies are lint rules outside tests: depguard's `go-git` list
  keeps go-git and go-billy in `gitfetch` (and `fakegit`), its
  `cache-backends` list keeps `internal/cache/local` and `internal/cache/s3`
  to `internal/cache/cache.go`, and a forbidigo pattern keeps
  `tar.NewWriter` in `treearchive` (and `internal/testing`). forbidigo's
  default `fmt.Print` pattern is spelled out beside it, since a `forbid`
  list replaces the default.
- `fieldalignment` fails a padded struct layout, test structs included; `just
  fix` reorders it in place.
- Never respell a dependency's anonymous struct type locally: `just fix`
  reorders the copy only. Append an inferred zero element, as fakegalaxy's
  `appendRow` does.
- A `//nolint` carries its reason by convention; `nolintlint` does not require
  one.

## Test conventions

- Tests sit beside the code, in-package by default; the collections e2e suite
  and a few `cache`, `fetch` and `treearchive` files are `package <pkg>_test`.
- Positive controls are mandatory: a detector that finds nothing passes a
  monopoly gate perfectly.
- Expected values are literals, never derived from the constant under test,
  which would move with it.
- Never `t.Parallel` a `captureStdIO` test, or a signature test measuring
  `runtime.MemStats` or wall time: both are process-wide.
- Lowering the solver's `oracleSeedCount` weakens `TestOracleMembership`, the
  completeness gate.
- S3 lock tests wait on events (`waitForLockEvent`), never elapsed time.
  `fakeS3` never verifies SigV4: `TestRequestURLSignedPathMatchesSentPath` and
  `TestAwsURIEncodeMatchesS3` do.

| Trap | Why it passes vacuously | Do instead |
| --- | --- | --- |
| Offline | `cfg.Offline` over a live client or a warm API cache never reaches the refusal | run over `fetch.NewOffline` after `Store.ClearCaches` |
| Secrets in JSON | `encoding/json` escapes `&`, `<`, `>`, so a needle spanning `&` never matches | search one parameter, with a positive control |
| Hand-assembled tar | without the two zero trailer blocks a short read fails on its own | end the fixture with both trailer blocks |
| Config | `/etc/ansible/ansible.cfg` stays reachable past `neutralizeAnsibleDiscovery` | assert only flag or env values unlike the defaults; no `:` in paths |
| galaxy.toml config | its `${VAR}` expand from the process environment | `t.Setenv` without `t.Parallel`, or pass a `projectSettings` value |
| S3 fault | `failNext` below `s3RetryMaxAttempts` is absorbed by the retry | arm `s3RetryMaxAttempts` or `-1` |
| Hang, stall or drip | the fault blocks server shutdown | end the request from the test |

Nothing in the suite dials a real host:

| Double | Models | Rules |
| --- | --- | --- |
| `fakegalaxy` | Galaxy v3 (`New`) or a hub (`NewAtBasePath`); v1 roles after `AddRole` | counts, then auth, then fault; wire names are local literals |
| `fakegit` | smart HTTP and ssh remotes, fixed commit times | counts, fault, then `RequireAuth`; validates nothing a repo carries |
| `faketree` | an in-memory `treearchive.Source` | builders and the collections suite's git client double |

<details markdown>
<summary>Signature testdata: how it was made and what a regeneration must keep</summary>

`internal/galaxy/signature/testdata` is the one `testdata` directory:
committed gpg 2.5.21 output, never generated at test time. It is public
material except `secret.*`, a discardable key proving `LoadKeyring` refuses
secret material. Keys are `gpg --quick-generate-key '<uid>' ed25519 sign
never` (the expired one `sign seconds=10`); signatures are `gpg -u <key>
--detach-sign [--armor]` over `manifest-a.json` or `manifest-b.json`.

A regenerated set keeps what tests read from the bytes: the expired key
exported inside its window, `sig-a-expsig.asc` with a five-second expiry, the
revoked key exported after its revocation, `keyring.asc` without the outsider,
and `signing-subkey.asc` as the only embedded primary-key binding. Shapes gpg
does not write live in `verify_test.go`'s `synthesizedBlobs`. A new
packet-bearing fixture needs a `gatedFixtures` row and restated
`committedPacketMeasurements` in `framing_test.go`.

</details>

## The documentation site

- `docs/` builds with [Zensical](https://zensical.org) through `uvx`, pinned
  as `ZENSICAL_VERSION` because it is pre-1.0.
- `just docs` serves `http://localhost:8000/go-galaxy/`, rebuilt on save;
  `just docs_build` fails on a dead link or anchor.
- Anchors slug the way GitHub slugs them (`toc.slugify`), so
  `servers-and-auth.md#--token` resolves on both.
- A link out of `docs/` is absolute (`https://github.com/greeddj/go-galaxy/blob/main/...`):
  the strict build refuses a relative one that leaves it.
- A new page needs a `nav` entry in `zensical.toml`, or it renders without
  warning and nothing links to it.
- Each `nav` section is one directory: `get-started/`, `guides/`,
  `reference/` and `internals/`, with `index.md` and `assets/` at the top.
  A page in one section links another as `../<section>/page.md`.
- A release archive ships the binary and `LICENSE` only, so no page or image
  under `docs/` has to be listed in `.goreleaser.yml`.
- `docs/index.md` repeats `README.md`'s opening paragraph: change both. The
  build writes `site/` and `.cache/`, both git-ignored.

## The benchmark harness

### testing/bench.sh

It times `ansible-galaxy` against `go-galaxy` over
`testing/requirements-{1,10,100}.yml` and `requirements-roles.yml`, every
command with `--no-deps`, so it compares fetch plus extract.

```bash
python3 -m venv .venv && .venv/bin/pip install ansible-core
go build -o ./dist/go-galaxy ./cmd/go-galaxy
docker compose -f testing/docker-compose.yaml up -d minio-svc   # for the s3-* scenarios
testing/bench.sh
```

It needs `hyperfine` and `python3` on `PATH`, plus the binary and `.venv`
above; without MinIO the S3 scenarios are dropped with a warning. Caches live
under `$TMPDIR`: not in `$HOME`, so a wipe spares yours, nor in the
repository, where extracted Go files would reach a linter.

| Knob | Default |
| --- | --- |
| `RUNS`, `WARMUP` | `5`, `1` |
| `SIZES` | `1 10 100` |
| `SCENARIOS` | `cold warm frozen s3-cold s3-warm s3-frozen roles-cold roles-warm` |
| `S3_ENDPOINT` | `http://127.0.0.1:9000` |
| `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` | `go-galaxy-bench`, `local-user`, `local-password` |

| Output in `dist/bench/` | Holds |
| --- | --- |
| `<scenario>-<N>.md` | one collection scenario at one size |
| `roles-cold.md`, `roles-warm.md` | the role scenarios |
| `resources-<N>.md` | peak RSS and bytes downloaded, from a separate single-run pass |
| `summary.md` | everything above, with tool versions and host |

### go-galaxy-benchmark

`cmd/go-galaxy-benchmark` narrows the comparison to collections in `cold` and
`warm`: `run` measures and writes `report.json`, `show` re-renders it as a
table or SVG. Flags and a sample run: [Benchmarks](../reference/benchmarks.md#go-galaxy-benchmark).

- It overrides only `GO_GALAXY_CACHE_DIR` and `TMPDIR`, so an exported
  `GO_GALAXY_S3_BUCKET` turns the run into an S3 run.
- Every measured command gets a closed stdin, so a prompting tool exits
  instead of looking hung.
- A failed run counts in `failed` with its `last_error`, outside `samples_ms`;
  an interrupt writes no report.

## Dependencies

`vendor/` is git-ignored and absent in CI; `just deps` regenerates it, and Go
refuses to build when `vendor/modules.txt` disagrees with `go.mod`. A new
dependency needs its depguard `allow:` entry and a `just deps` run.

go-git, go-billy and `x/crypto/ssh` are imported only by `gitfetch` and
`fakegit`. `just check` never runs `just deps`: a gate that rewrites `go.mod`
first can only agree with itself.

| Bump | Can fail | Then |
| --- | --- | --- |
| Go toolchain | `collectionbuild`'s `TestBuildGoldenDigest` (`compress/flate` bytes are not promised) | if `TestBuildStructure` and `rolebuild`'s `TestBuildArtifactShape` pass, take the new digest |
| Go toolchain | `TestMetaHeaderCeilingIsWhatArchiveTarReads` | re-derive the probe floor |
| Masterminds/semver | `TestVerSetDifferentialAgainstCheck`, `TestVerSetGroundTruthRows` | fix the mirrored grammar in `versetbuild.go`; `Check` stays the authority |
| ProtonMail/go-crypto | `framing_test.go` measurements | a gate premise changed: investigate before moving a ceiling |

Never build with `-tags v5`: go-crypto's `!v5` constraint is what refuses v5
signatures and keys before a length is read (`TestV5ParsingStaysDisabled`).

## Conventions

- A comment is at most three lines. How a package works belongs in
  [How it works](index.md) and its sibling pages, the boundary it enforces in
  [Security boundaries](boundaries.md); keep both true after a change.
- Comments state constraints and reasons, not narration.
- Only hyphen-minus, everywhere, enforced by test.
- `README.md` is a landing page and reference lives in `docs/`: a behavior
  change lands with the page that describes it.

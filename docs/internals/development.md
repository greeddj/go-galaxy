# Development

Everything here runs from a checkout with Go, `git` and
[just](https://github.com/casey/just). `just lint` also needs golangci-lint at
the pinned release, and `just docs` and `just docs_build` need
[uv](https://docs.astral.sh/uv/), whose `uvx` fetches Zensical. The test suite
needs no network, container runtime or Python. The benchmark harness needs
Python, for `ansible-galaxy`.

## Recipes

| Recipe | Runs |
| --- | --- |
| `just test` | `go test ./...` |
| `just check` | `go vet`, staticcheck, govulncheck, fieldalignment, actionlint |
| `just lint` | golangci-lint, refusing any release but `GOLANGCI_LINT_VERSION` |
| `just fix` | `go fix` and `fieldalignment -fix` |
| `just deps` | `go mod tidy && go mod vendor`, after any dependency change |
| `just build` | check, lint and test, then `dist/go-galaxy` and `dist/go-galaxy-benchmark` |
| `just oci` | a local linux/amd64 image from `Dockerfile` alone, not `Dockerfile.alpine`, built by podman unless you name another tool (`just oci docker`) |
| `just docs`, `just docs_build` | the [documentation site](#the-documentation-site) |

## Running the tests

```bash
# what CI runs
just check && just lint
go test -v -race -coverprofile=coverage.txt ./...

# one package, one test, benchmarks, a fuzz target past its seeds
go test ./internal/galaxy/archive/ -run 'TestProbeTarGzUsesTheProbeSizedDecompressor'
go test ./internal/galaxy/solver -bench . -run '^$'
go test ./internal/galaxy/solver -fuzz FuzzSolve -fuzztime 60s
```

- Run with `-race` before calling concurrency work done.
- The `go tool` binaries come from the `tool` directive in `go.mod`.
- `just check` and `ci.yml` both pass `-shellcheck= -pyflakes=`. actionlint
  otherwise runs those tools whenever it finds them on `PATH`, so a check
  could fail on a machine that has them, such as GitHub's runner with
  shellcheck, and pass on one that does not.
- The six fuzz targets run only their seed corpora under `go test`.

> [!WARNING]
> Do not run the suite as root: tests asserting a permission refusal skip
> there, and the coverage vanishes silently.

## CI

| Workflow | Trigger | Runs |
| --- | --- | --- |
| `ci.yml` | push to `main` (not `**.md` alone), pull request, `workflow_call` | the check commands, pinned golangci-lint, the `-race` suite, a non-failing coverage upload |
| `docs.yml` | push to `main` touching `**.md`, `docs/**`, `zensical.toml` or the `Justfile` | `go test ./internal/proseaudit/`, `just docs_build` |
| `release.yml` | a `v*` tag | `ci.yml` first, then GoReleaser, attestation, the major tag move, the [documentation site](#the-documentation-site) to GitHub Pages |
| `action.yml` | a change to `action.yml` or itself; manual | the composite action against galaxy.ansible.com, and offline over a lockfile that does not load, where it must fail at its cache-key step with go-galaxy on `PATH` and nothing installed |

- `.goreleaser.yml` runs no `before` hooks: the gate job ran the suite, and
  `go mod tidy` would rewrite the reviewed dependency set.
- A tag with `-` is a prerelease.
  [Cutting a release](https://github.com/greeddj/go-galaxy/blob/main/CONTRIBUTING.md#cutting-a-release)
  lists what it skips.
- One binary, two images: `Dockerfile` builds the distroless one
  (`<version>`, `<version>-distroless`, `latest`) and `Dockerfile.alpine` the
  Alpine one (`<version>-alpine`); both run go-galaxy as uid 65532.
- The major tag (`v1`) is the one tag that does not fix a state, hence
  `--match` in the Justfile's `git describe`.

Release notes come from [commit subjects](https://github.com/greeddj/go-galaxy/blob/main/CONTRIBUTING.md#commit-subjects).
[Verifying a release](../guides/security.md#verifying-a-release) shows how to check one.

## The repository audits itself

Every gate is an ordinary test: three test-only packages plus five audit
files inside the package they gate. A gate fails when a function or file it
names is gone, so a rename cannot switch it off; only the dash and
comment-length gates skip when `git` cannot list the tree.

| Gate | Enforces | To satisfy it |
| --- | --- | --- |
| `proseaudit`: dashes | no U+2014 or U+2013 in a tracked file | type `-`; a test spells one as bytes or `\uXXXX` |
| `proseaudit`: line citations | `foo_test.go:NNN` only on a failure-reportable line; no production line | renumber after inserting lines, or cite an identifier |
| `proseaudit`: comment length | no comment block over three lines in Go, `.sh`, YAML, `Justfile`, `Dockerfile`, `Dockerfile.*`, `.gitignore` | move reasoning to [How it works](index.md) and its sibling pages, or to [Security boundaries](boundaries.md); never split one comment into blocks |
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

**Probe budget.** `helpers.ArchiveProbeMaxBytes` (5 MiB) sits above the
meta-header ceiling, 4,196,352 bytes: the most `archive/tar` reads before its
first header. `TestArchiveProbeMaxBytesClearsTheMetaHeaderCeiling` bounds it
and `TestMetaHeaderCeilingIsWhatArchiveTarReads` measures it. Re-derive the
ceiling if the second fails.

</details>

The rest of a new snapshot bucket (`ensureMaps`, `jsonBuckets`, the schema
version) is in [Snapshot](cache.md#snapshot). A new sentinel's exit-code rules
are in [Adding a sentinel](http-output-exit-codes.md#adding-a-sentinel).

## Lint

`.golangci.yml` runs `default: all` with a short disable list and no test-file
exclusions, so expect linters most projects leave off, in tests too.

depguard allows only the standard library, `go/ast`, `go/parser`, `go/token`,
this module and the direct dependencies, so any other import fails. Outside
tests, three more rules keep an import or a call in one place:

| Kept in one place | Allowed in | Rule |
| --- | --- | --- |
| go-git and go-billy | `gitfetch` and `fakegit` | depguard's `go-git` list |
| `internal/cache/local` and `internal/cache/s3` | `internal/cache/cache.go` | depguard's `cache-backends` list |
| `tar.NewWriter` | `treearchive` and `internal/testing` | a forbidigo pattern |

- forbidigo's default `fmt.Print` pattern is spelled out beside that pattern,
  since a `forbid` list replaces the default.
- `fieldalignment` fails a padded struct layout, test structs included. `just
  fix` reorders it in place.
- Never spell a dependency's anonymous struct type out locally: `just fix`
  reorders your copy's fields, and the copy stops being the dependency's type.
  Grow such a slice through a generic helper that appends a zero element, as
  fakegalaxy's `appendRow` does.
- A `//nolint` carries its reason by convention. `nolintlint` does not require
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
| Offline | `cfg.Offline` over a live client or a warm API cache never reaches the refusal; a live git double serves a fetch that should have been refused | run over `fetch.NewOffline` after `Store.ClearCaches`; for git, assert zero calls on the counting double or wire `gitsource.Offline` |
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
| `fakegit` | smart HTTP and ssh remotes, fixed commit times; an ssh session ends only after the client's EOF, which git does not wait for | counts, fault, then `RequireAuth`; validates nothing a repo carries |
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
- `just docs` serves `http://localhost:8000/go-galaxy/`, rebuilt on save.
  `just docs_build` fails on a dead link or anchor. `docs.yml` runs it only
  once a change is on `main`, so run it before pushing a change under `docs/`.
- The site is published from a release tag alone: `release.yml` builds it
  from the tag and deploys it to GitHub Pages, so it describes the latest
  release, not `main`, and a prerelease leaves it as it was. That needs Pages
  set to deploy from GitHub Actions and the `github-pages` environment
  admitting a `v*` tag alone: GitHub creates it with a rule for `main`,
  removed so that no run on `main` can publish the site.
- Anchors slug the way GitHub slugs them (`toc.slugify`), so
  `requirements.md#galaxytoml` resolves on both. Rewording a heading
  changes its anchor: update every link to it. The strict build catches those
  under `docs/`; grep `README.md`, `CONTRIBUTING.md`, `.goreleaser.yml` and Go
  comments for the rest.
- A link out of `docs/` is absolute (`https://github.com/greeddj/go-galaxy/blob/main/...`):
  the strict build refuses a relative one that leaves it.
- A new page needs a `nav` entry in `zensical.toml`, or it renders without
  warning and nothing links to it.
- Each `nav` section is one directory: `get-started/`, `guides/`,
  `reference/` and `internals/`, with `index.md` and `assets/` at the top.
  A page in one section links another as `../<section>/page.md`.
- A release archive ships the binary and `LICENSE` only, so no page or image
  under `docs/` has to be listed in `.goreleaser.yml`.
- The build writes `site/` and `.cache/`, both git-ignored.

The pages follow a few rules:

- Each rule has one home page. Every other page gives it a clause and a link.
- An example given in both formats leads with `galaxy.toml`: the first tab,
  or the left column of a side-by-side comparison whose lines are short
  enough not to scroll.
- `README.md` is a landing page, and the reference lives under `docs/`.
  `README.md` uses only syntax GitHub renders. Site pages may use
  admonitions, tabs, cards and code annotations.
- Pages under `internals/` stay short and link the user pages for behavior.
- Release history goes to [Upgrading](../reference/upgrading.md) alone, by the
  [house rules](https://github.com/greeddj/go-galaxy/blob/main/CONTRIBUTING.md#house-rules).
- A behavior change lands with the page that describes it.

`README.md` repeats parts of the site so that it reads on its own on GitHub.
Change both sides in one commit:

| `README.md` | Repeats |
| --- | --- |
| The tagline, opening paragraph, benchmark chart and caption, and the "Why go-galaxy" list | `docs/index.md` |
| The binary snippet and the container alias | The Binary and Container tabs under Quick start's [Install](../get-started/getting-started.md#install) |
| Its Quick start section | The example files and commands of [Quick start](../get-started/getting-started.md) |
| Its header, `docs/assets/logo-banner.svg` and `logo-banner-white.svg` | A copy of `logo.svg` and `logo-white.svg`, beside the wordmark |

## The benchmark harness

`cmd/go-galaxy-benchmark` compares `ansible-galaxy` and go-galaxy on
collections in `cold` and `warm`: `run` measures and writes `report.json`,
`show` re-renders it as a table or SVG. Setup, flags and a sample run are on
[Reproduce](../reference/benchmarks.md#reproduce).

- It passes the caller's environment through and overrides only each tool's
  cache, temporary and install paths.
  [Reproduce](../reference/benchmarks.md#reproduce) warns what that lets in.
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
| Go toolchain | `TestMetaHeaderCeilingIsWhatArchiveTarReads` | re-derive the meta-header ceiling, as under Probe budget above |
| Masterminds/semver | `TestVerSetDifferentialAgainstCheck`, `TestVerSetGroundTruthRows`, `TestCanonicalConstraintPreservesMeaning` | fix the mirrored grammar in `helpers/constraint.go` or `versetbuild.go`; `Check` stays the authority |
| ProtonMail/go-crypto | `framing_test.go` measurements | a gate premise changed: investigate before moving a ceiling |
| `go get -u tool` | `go tool actionlint` stops building: actionlint v1.7.12 compiles against `go.yaml.in/yaml/v4` `v4.0.0-rc.3` | pin `go.yaml.in/yaml/v4` back to `v4.0.0-rc.3` |

Never build with `-tags v5`: go-crypto's `!v5` constraint is what refuses v5
signatures and keys before a length is read (`TestV5ParsingStaysDisabled`).

## Conventions

Comments state constraints and reasons, not narration. Keep them, and the
pages they point to, true after a change.

# Contributing

Two things here are worth reading before your first commit, because neither is
recoverable after the fact: how a commit subject decides where the change is
published, and how a tag decides what the release page says.

Everything about running the tests, the gates this repository points at its own
source, and the lint configuration is in
[Development](docs/internals/development.md). This document is the part that is about
what you write rather than what you run.

## Before you push

```bash
just check     # go vet, staticcheck, govulncheck, fieldalignment, actionlint
just lint      # golangci-lint, at exactly the pinned release
just test      # go test ./...
```

Run the suite with `-race` before considering any concurrency work done, and
do not run it as root: several tests assert that a read or a write is refused
by file permissions, and a process not bound by them skips those rather than
failing.

Bumping golangci-lint means editing two spellings in one commit, the
`Justfile`'s `GOLANGCI_LINT_VERSION` and the version input in
`.github/workflows/ci.yml`. A test refuses the two disagreeing.

## Commit subjects

The release notes are grouped out of commit subjects, so the subject line is
the only thing deciding whether a change is published and where. The body is
never read: a `BREAKING CHANGE:` footer reaches nothing, and a breaking change
that does not mark its subject is published as an ordinary one.

Subjects follow Conventional Commits - `type(scope)!: summary`, with the scope
and the `!` both optional. The rules below are the ones `changelog:` in
`.goreleaser.yml` actually implements; that file is the ground truth, and it
explains why each pattern is spelled the way it is.

| Subject | Published under |
| :-- | :-- |
| `feat: ...`, `feat(cache): ...` | Features |
| `fix: ...`, `fix(galaxy/collections): ...` | Bug fixes |
| any type with `!` before the colon | Breaking changes |
| `docs: ...`, `test: ...`, `chore: ...`, with or without a scope | not published |
| `Merge pull request ...`, `Merge branch ...` | not published |
| anything else - `ci:`, `build:`, `perf:`, `refactor:`, `style:`, `revert:` | Others |

Four consequences that do not follow from reading the table.

**A `!` beats the type, in both directions.** A commit lands in the first group
whose pattern matches it, and the breaking pattern is declared first, so
`feat!:` is published under Breaking changes rather than Features. In the other
direction, the exclusions match a type followed immediately by its colon, which
a `!` interrupts: `chore!:` and `docs!:` are *not* dropped, they are published
under Breaking changes. That is deliberate. A breaking change has to be
announced whatever its type, and marking one as a chore should not be able to
hide it.

**The excluded types are excluded only when spelled plainly.** `chore:` and
`chore(deps):` are dropped; `chore(deps)!:` is not.

**Scopes may carry slashes.** They are matched loosely rather than as single
words, because ours are package paths: `fix(galaxy/collections):` and
`docs(cache):` both work.

**Dependabot is already configured to land correctly.** Its Go module updates
are prefixed `chore(deps)` and drop out of the notes; its Actions updates are
prefixed `ci(deps)` and appear under Others. See `.github/dependabot.yml`.

## House rules

**Hyphen-minus only.** No em dash (U+2014), no en dash (U+2013), anywhere -
prose, code, comments and commit messages alike. A test enumerates every
committed file and fails naming each occurrence; commit messages are the same
rule as a matter of house style, since nothing can check them after the fact.
Type `-`.

**The commit describes itself.** State the constraint and the reason, and say
what was measured. Do not reference anything the repository does not contain:
no plan documents, no phase numbers, no task identifiers. Somebody reading
`git log` in two years has the code and the message and nothing else.

**Subject in the imperative, lower case after the colon, no trailing period.**
Wrap the body at 72 columns or so.

**A change someone must act on gets a row in `docs/reference/upgrading.md`.** A `!`
subject, a new lockfile or snapshot schema, a renamed flag or variable: add
its row to the newest "From" section in the same commit. No other page tells
release history, so a change without its row reaches users only through the
release notes.

## Cutting a release

A release is a tag push. `.github/workflows/release.yml` fires on `v*`, runs
the whole CI suite as its gate, and only then publishes; a red gate publishes
nothing at all.

The tag message is load-bearing. The release page opens with the tag's own
body, so what a release says about itself is written when it is cut. Three
things go wrong quietly here.

**The tag must be annotated or signed** - `git tag -a` or `git tag -s`. For a
lightweight tag the body falls back to the body of the commit the tag points
at, which is a paragraph of implementation reasoning the release page never
asked for.

**One `-m` is not enough.** The first `-m` becomes the tag's subject, and the
body is everything after the first line - so a tag created with a single `-m`
has an empty body and renders an empty header. Use `git tag -a vX.Y.Z -F file`
(the file's first line becomes the subject and does not reach the page) or pass
`-m` twice.

**Write the body without apostrophes.** GoReleaser strips every `'` while
rendering the tag body, so `the ansible layout` survives and `ansible's layout`
arrives as `ansibles layout`. The generated changelog below the header is not
affected. Check what will actually be published before pushing:

```bash
git tag -l --format='%(contents:body)' vX.Y.Z
```

Versions are semver. A tag carrying a prerelease part - `v1.2.0-rc.1` -
publishes as a GitHub prerelease and deliberately does not update the Homebrew
tap, so release candidates do not reach anyone running `brew upgrade`.

## Pull requests

Changes from outside arrive as pull requests against `main`. The full suite
runs on every pull request whatever it touches, so a check required for merging
always reports, including on a branch that changes only documentation.

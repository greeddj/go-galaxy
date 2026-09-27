# Contributing

Two things here are worth reading before your first commit, because neither is
recoverable after the fact: how a commit subject decides where the change is
published, and how a tag decides what the release page says.

Running the tests, the gates this repository points at its own source, and the
lint configuration are in [Development](docs/internals/development.md). This
document covers what you write, plus the commands to run before a push.

## Before you push

```bash
just check          # go vet, staticcheck, govulncheck, fieldalignment, actionlint
just lint           # golangci-lint, at exactly the pinned release
go test -race ./...
just docs_build     # after a docs change: a dead link or anchor fails it
```

The first three are what CI runs. No workflow builds the site, so a dead link
reaches `main` unless `just docs_build` catches it here. Never run the suite as
root ([Running the tests](docs/internals/development.md#running-the-tests) says
why). A golangci-lint bump edits the `Justfile` and `.github/workflows/ci.yml`
in one commit, or a gate fails
([The repository audits itself](docs/internals/development.md#the-repository-audits-itself)).

## Commit subjects

The release notes are grouped out of commit subjects, so the subject line is
the only thing deciding whether a change is published and where. The body is
never read: a `BREAKING CHANGE:` footer reaches nothing, and a breaking change
that does not mark its subject is published as an ordinary one.

Subjects follow Conventional Commits - `type(scope)!: summary`, with the scope
and the `!` both optional. The rules below are the ones `changelog:` in
`.goreleaser.yml` implements, and that file is the ground truth. The
consequences after the table explain its spelling.

| Subject | Published under |
| :-- | :-- |
| `feat: ...`, `feat(cache): ...` | Features |
| `fix: ...`, `fix(galaxy/collections): ...` | Bug fixes |
| any type with `!` before the colon | Breaking changes |
| `docs: ...`, `test: ...`, `chore: ...`, with or without a scope | not published |
| `Merge pull request ...`, `Merge branch ...` | not published |
| anything else - `ci:`, `build:`, `perf:`, `refactor:`, `style:`, `revert:` | Others |

Three consequences that do not follow from reading the table.

**A `!` beats the type, in both directions.** A commit lands in the first group
whose pattern matches it, and the breaking pattern is declared first, so
`feat!:` is published under Breaking changes rather than Features. In the other
direction, the exclusions match a type followed immediately by its colon, which
a `!` interrupts: `chore!:`, `docs!:` and `chore(deps)!:` are *not* dropped but
published under Breaking changes. That is deliberate. A breaking change has to
be announced whatever its type, and marking one as a chore should not be able
to hide it.

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

**A change someone must act on gets a row in [Upgrading](docs/reference/upgrading.md).**
A new lockfile or snapshot schema, a renamed flag or variable and most `!`
subjects are such changes. Add the row in the same commit, to the section named
after the latest release tag: `From vX.Y.x` when the latest tag is `vX.Y.Z`.
When the newest section names an older release, open the new one. A changed
exit code goes into that section's "Exit codes that changed" table, whose
columns are Situation, Was and Now. Any other change goes into its main table.
A relaxation nobody must act on, such as an input now accepted or ignored that
used to be refused, gets no row, even when its subject carries `!`. No other
page tells release history, so a change without its row reaches users only
through the release notes.

## Cutting a release

A release is a tag push. `.github/workflows/release.yml` fires on `v*`, runs
the whole CI suite as its gate, and only then publishes. A red gate publishes
nothing at all.

1. Pick the version. Versions are semver. A tag carrying a prerelease part,
   such as `v1.2.0-rc.1`, publishes as a GitHub prerelease and moves nothing
   users track: the Homebrew cask is not updated, the image gets no `latest`
   tag, and the major tag (`v1`) stays where it is. So a release candidate
   reaches no one running `brew upgrade`, pulling `latest` or using `@v1`.
2. For a release, not a prerelease, move the docs' pinned-release examples to
   the new version in a commit of their own, before the tag, and push it to
   `main`. They live in `README.md`, `docs/get-started/getting-started.md`,
   `docs/guides/ci.md` and `docs/guides/security.md`: image tags, `@vX.Y.Z`,
   `version:`, `RELEASE`, the cache `prefix`, download URLs, archive names and
   the verify recipe's `tag=`. Searching those four files for the old number
   finds every one.
   Leave `docs/reference/upgrading.md` and the version constraints in
   `docs/guides/requirements.md` alone.
3. Write the release text to a file outside the checkout: a subject line, a
   blank line, then the body the release page opens with, without apostrophes.
4. Check out the commit to release. For a release, that is the step 2 commit
   or a later one. Tag it with `git tag -a vX.Y.Z -F <file>`, or with `-s` in
   place of `-a` to sign it.
5. Check the body: `git tag -l --format='%(contents:body)' vX.Y.Z` prints what
   the page opens with. It must not be empty and must hold no `'`.
6. Push the tag: `git push origin vX.Y.Z`.

The tag message is load-bearing. The release page opens with the tag's own
body, so what a release says about itself is written when it is cut. Three
things go wrong quietly here.

**The tag must be annotated or signed** - `git tag -a` or `git tag -s`. For a
lightweight tag the body falls back to the body of the commit the tag points
at, which is a paragraph of implementation reasoning the release page never
asked for.

**One `-m` is not enough.** Git splits a tag message at its first blank line.
The part before it is the subject, which never reaches the page, and the rest
is the body. A single `-m`, or a file with no blank line in it, leaves the
body empty, and the release page opens with an empty header. Use `-F` with a
file laid out as in step 3, or pass `-m` twice.

**Write the body without apostrophes.** GoReleaser strips every `'` while
rendering the tag body, so `the ansible layout` survives and `ansible's layout`
arrives as `ansibles layout`. The generated changelog below the header is not
affected. The check in step 5 shows the body before that stripping, so look for
`'` there yourself.

## Pull requests

Changes from outside arrive as pull requests against `main`. The full suite
runs on every pull request whatever it touches, so a check required for merging
always reports, including on a branch that changes only documentation.

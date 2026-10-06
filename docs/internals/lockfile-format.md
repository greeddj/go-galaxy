# Lockfile format

`internal/galaxy/lockfile` reads, validates and writes `galaxy.lock`, the pins
a frozen install trusts. How to use the file: [Lockfile](../guides/lockfile.md).

## Schema versions

| `schema_version` | Written when the file holds | Constant | A release that predates it |
| ---: | --- | --- | --- |
| 1 | None of the entries below | `SchemaVersion` | - |
| 2 | A git collection | `SchemaVersionGit` | Refuses the file rather than read a repository URL as a Galaxy server |
| 3 | A role | `SchemaVersionRoles` | Refuses the file rather than install the collections and skip the roles |
| 4 | A url collection or url role | `SchemaVersionURL` | Refuses the file |
| 5 | A Galaxy collection | `SchemaVersionDownloadURL` | Refuses the file, which carries `download_url` |

`SchemaVersionFor` picks the highest feature present, and `canonicalize` sets
it on every write, so no producer chooses it. `Load` accepts any schema from 1
to 5 at least as high as each entry needs. A Galaxy entry below 5 lacks the
required `download_url` and is refused with a message naming `go-galaxy lock`.

## Entry kinds

| Entry | `type` | Pinned by | Also carries | Refused on load |
| --- | --- | --- | --- | --- |
| Galaxy collection | none | `sha256`, `download_url` | Server `source` | `ref`, `commit`, `subdir` |
| git collection | `git` | `commit` | Repository `source`, `ref` as asked, `subdir` | `sha256`, `download_url` |
| url collection | `url` | `sha256` of the origin bytes | Tarball `source` | `ref`, `commit`, `subdir`, `download_url` |
| Galaxy role | `galaxy` | `commit` | `galaxy` name, server `source`, `repository`, qualified `ref` | `sha256` |
| git role | `git` | `commit` | Repository `source`, `ref` as asked | `sha256`, `galaxy`, `repository` |
| url role | `url` | `sha256` of the origin bytes | Tarball `source` | `ref`, `commit`, `galaxy`, `repository` |

Every entry also has a `name`, a `version` and optional `deps`.

- A git pin has no `sha256`, since a rebuild's gzip bytes depend on the
  toolchain; a url pin's bytes are the origin's own.
- A Galaxy `sha256` may be empty, which keeps digest-less servers usable.
  Otherwise it is 64 lowercase hex digits: a malformed one from the server
  fails `lock` (`ErrMalformedArtifactSHA256`, exit 7), and one in the file
  fails `Load` (`ErrLockfileInvalid`, exit 6).
- A role `version` is not semver, since a branch is legal; a url role's
  default is the sha's first 12 hex digits.
- A Galaxy role's `ref` is `refs/tags/<tag>` for a listed tag, else
  `refs/heads/<name>`, so a same-named branch is never fetched instead.
- The file-level `server` is provenance: a source-less entry takes the run's
  server. `Hash` covers it, so a `server_list` change that moves the default
  (first) server is drift.

## Loading

`File.validate` and `validateRoles` re-parse each repository, tarball and
download URL, ref and subdir, which must round-trip unchanged, and hold names,
versions, commits and url digests to their alphabets: a lockfile is
repository content. A name is checked before any message prints it, and the
server and a Galaxy source, which lock prints as written, may carry no control
character, since a newline could forge an output line.

They also refuse pins that contradict each other. A git collection, git role
or Galaxy role entry whose `ref` is a commit must pin that same commit, since
`--frozen` holds a git collection's or git role's ref to the requirement and
then installs its `commit`. No two url collection entries share a `source`,
since one tarball is one collection and `--frozen` finds a url root's entry by
source alone; two url roles may, as two role names may install one tarball.
That refusal names both entries, never the source, which may carry a query.

Every collection needs an exact version, or `"*"` would let `--frozen` take the
server's highest. `Save` does not validate: `buildLockfile` checks each
version with `helpers.IsExactVersion` and keys entries by fqdn.

| Outcome | Error | Callers |
| --- | --- | --- |
| Absent | Bare `fs.ErrNotExist` (`IsNotExist`) | `hash` and `outdated` fall back; `lock` keeps no pin, and `lock --dry-run` diffs against nothing |
| Absent, required | `LoadRequired`, or `RequiredError` over a `Load` already made, returns `ErrLockfileMissing` naming the path, exit 6 | `--frozen`, `lock --check`, `tree`, `explain` |
| Any other failure | Wraps `helpers.ErrLockfileInvalid`, exit 6 | Fatal, `lock --check` included; a plain `lock` and `lock --dry-run` warn and keep no pin, the metrics report omits the hash |

`Load` `Stat`s the path first, following a symlink, and refuses anything but
a regular file, a directory or fifo included, as `ErrLockfileInvalid` before
any open: a fifo would block `install --frozen` under the cache lock. A failed
`Stat` passes, so the read reports absence as before.

A new failure arm in `Load` must wrap `ErrLockfileInvalid`, never
`fs.ErrNotExist`: callers read that as absence, and a bare one reaching the
exit-code classifier exits 2.

## Git requirements and their entries

`MatchGitRequirements` decides which git requirement answers for each git
entry, so one entry is never judged by two: `--frozen` holds each git root to
its share (`GitMatch.Err`), `lock` keeps the commit the entries it owns share,
and `tree` and `explain` list it.

- A requirement is a candidate for a git entry locked from its repository at
  its subdir or an immediate child of it, and, when it names a collection,
  for that collection's entry alone.
- Of the candidates asking for the entry's ref, the one at the entry's own
  subdir owns it before the one at its parent. `prepareRoots` refuses two
  requirements at one repository and subdir, so no tie is left.
- An entry with candidates but no owner is a ref mismatch charged to its
  nearest candidate. An entry with no candidate answers to no requirement,
  and `--frozen` still installs it.
- A requirement fails on its first mismatch by entry name, else when it owns
  no entry. So a ref change is refused when the changed requirement owns no
  entry at its new ref, even when a new requirement at the old ref takes every
  entry it locked.

The rule reads the lockfile alone, so two edits still pass: a changed
requirement that owns, at its new ref, an entry another requirement locked
(a file a plain `lock` refuses as one collection asked for twice), and
requirements that exchange refs over one directory, which a file `lock` wrote
can look exactly like. `lock --check` compares such a file with what `lock`
writes for the new requirements.

The rule cannot see commits either. Two overlapping requirements of one ref
can have pins at different commits. When the child's directory changed
between them, to another collection or to a directory holding collections one
level down, the child owns every entry there, and a parent that locked nothing
else owns none: `--frozen` refuses that file, exit 6, though `lock` wrote it.
`lock --refresh` locks both at one commit, and the file then passes, or `lock`
refuses the requirements: one collection asked for twice, nothing under the
parent's subdir, or a named parent's collection no longer there.

## Canonical bytes

`Save` and `Hash` share `marshal` over `canonicalClone`: two-space indent,
collections and roles sorted by name, each `deps` sorted but never
deduplicated, the schema re-derived. `go-galaxy hash` prints the SHA256 of
those exact bytes, so the layout is a contract: changing it moves every CI
cache key. `TestSaveEmitsCanonicalBytes` pins the bytes literally.

`lockfile.Compare` backs `lock --check` and `lock --dry-run`, and
`Compare(a, b).Empty()` holds exactly when `a.Hash() == b.Hash()`
(`TestCompareEmptyMatchesHashEquality`). It compares every field but the name
it keys by, plus `server`, with deps as a multiset.

> [!WARNING]
> A field added to `Entry` or `RoleEntry` must join `sameEntry` or
> `sameRoleEntry`, `Fields`, and `comparedFieldCount` or
> `comparedRoleFieldCount`; a `File` field joins `Diff`. Otherwise
> `lock --check` passes a file `lock` would rewrite.

## download_url and frozen installs

A Galaxy entry's `download_url` lets a frozen install fetch a cache miss
without a metadata request, unless the run verifies signatures
([Plan construction](install-pipeline.md#plan-construction)). Behavior:
[Install from the lockfile](../guides/lockfile.md#install-from-the-lockfile).

`lock` refuses a `download_url` that carries a query or leaves its server's
origin, and `--frozen` re-checks the origin before any request and the
served artifact's `MANIFEST.json` identity before it is cached.
`lock` drops a fragment, since no request carries it. Rules, sentinels and
exits: [Loading the lockfile](boundaries.md#loading-the-lockfile).

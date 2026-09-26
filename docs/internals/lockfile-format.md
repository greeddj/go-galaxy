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
  server. `Hash` covers it, so a `server_list` reorder is drift.

## Loading

`File.validate` and `validateRoles` re-parse each repository, tarball and
download URL, ref and subdir, which must round-trip unchanged, and hold names,
versions, commits and url digests to their alphabets: a lockfile is
repository content. A name is checked before any message prints it, since a
newline could forge an output line.

Every collection needs an exact version, or `"*"` would let `--frozen` take the
server's highest. `Save` does not validate: `buildLockfile` checks each
version with `helpers.IsExactVersion` and keys entries by fqdn.

| Outcome | Error | Callers |
| --- | --- | --- |
| Absent | Bare `fs.ErrNotExist` (`IsNotExist`) | `hash` and `outdated` fall back; `lock --dry-run` diffs against nothing |
| Any other failure | Wraps `helpers.ErrLockfileInvalid`, exit 6 | Fatal; `lock --dry-run` warns, the metrics report omits the hash |
| Absent, required | `LoadRequired` returns `ErrLockfileMissing` naming the path, exit 6 | `--frozen`, `lock --check`, `tree`, `explain` |

A new failure arm in `Load` must wrap `ErrLockfileInvalid`, never
`fs.ErrNotExist`: callers read that as absence, and a bare one reaching the
exit-code classifier exits 2.

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

A frozen install resolves nothing and asks no Galaxy API.
`resolveFromLockfile` holds each root to its entry (`verifyRootsAgainstLockfile`,
`ErrLockfileMismatch`) and builds the graph from the file. On a cache miss,
`versionMetadata` hands over the locked `download_url` and `sha256` without a
request.

A verifying run passes `lockedURLs` false and fetches version metadata, which
its signatures ride on. Behavior:
[Install from the lockfile](../guides/lockfile.md#install-from-the-lockfile).

| `lock` refuses a download URL that | Sentinel | Exit | Because |
| --- | --- | --- | --- |
| Carries a query | `ErrDownloadURLQuery` | 5 | A presigned capability would be committed, then expire |
| Leaves its server's origin, or whose path does not end in `/<ns>-<name>-<version>.tar.gz` | `ErrDownloadURLNotServerArtifact` | 5 | Its bytes fill the [cache slot](cache.md#artifact-cache-scoped-by-server-not-by-content) every later install of that version reads |

A fragment is dropped, since no request carries it. `--frozen` re-checks origin
and path in `checkLockedDownloadURLs` before any request, as
`ErrLockfileInvalid`, exit 6; `Load` cannot, because a `server_list` id
resolves only from the run's configuration. The full boundary:
[Loading requirements.yml and the lockfile](boundaries.md#loading-requirementsyml-and-the-lockfile).

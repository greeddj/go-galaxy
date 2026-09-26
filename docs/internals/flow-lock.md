# lock flow

`lock` resolves the requirements file as install does without `--frozen`,
builds the lockfile in memory and writes it, previews it under `--dry-run` or
compares it under `--check`. Options:
[`lock`](../cli.md#lock); code meanings: [Exit codes](../exit-codes.md); the
file itself: [Lockfile format](lockfile-format.md).

## Overview

```mermaid
flowchart TD
  L1(["Shared setup: lock held,<br/>snapshot loaded"]) --> L2["loadRoots"]
  L2 -->|"refused"| X2(["exit 2"])
  L2 --> L3["resolveCollectionsInternal,<br/>as install resolves"]
  L3 -->|"failed"| XR(["exit 1, 2, 3, 4,<br/>5 or 7 by cause"])
  L3 --> L4["resolveRoles,<br/>as install resolves"]
  L4 -->|"failed"| XR
  L4 --> L5["buildLockfile, see<br/>Building the lockfile"]
  L5 -->|"refused"| XB(["exit 1, 2, 4,<br/>5 or 7 by cause"])
  L5 --> L6["write, preview or gate,<br/>see its diagram"]
  L6 --> XO(["exit 0, 1, 2,<br/>4 or 6 by cause"])
```

`lockWithState` runs under [Shared setup](commands.md#shared-setup) with the
banner `Resolving for lockfile`. Resolution and source discovery are install's,
drawn on [install flow](flow-install.md): the same
`resolveCollectionsInternal` and `resolveRoles`, without `newVerifyContext` or
`planCollections`. It installs nothing but saves the snapshot, so later runs
replay the resolve.

## Building the lockfile

```mermaid
flowchart TD
  B1["roleLockfileEntries:<br/>roles in name order"] -->|"a role not pinned"| X2(["exit 2"])
  B1 --> B2{"next collection:<br/>version exact?"}
  B2 -->|"no"| X2
  B2 -->|"yes"| B3{"source kind?"}
  B3 -->|"git or url, unpinned"| X2
  B3 -->|"Galaxy"| B6["Galaxy entry,<br/>see below"]
  B3 -->|"git"| B4["git entry: repository,<br/>ref, commit, subdir"]
  B3 -->|"url"| B5["url entry: URL,<br/>origin sha256"]
  B6 -->|"refused"| XG(["exit 1, 3, 4,<br/>5 or 7 by cause"])
  B6 --> B11{"collections left?"}
  B4 --> B11
  B5 --> B11
  B11 -->|"yes"| B2
  B11 -->|"no"| B12(["SchemaVersionFor<br/>picks the schema"])
```

A Galaxy entry, in `galaxyLockfileEntry`:

```mermaid
flowchart TD
  B7["loadCollectionMetadata"] -->|"unavailable"| X4(["exit 4"])
  B7 -->|"404: gone<br/>from its server"| X3(["exit 3"])
  B7 -->|"another status<br/>on a version"| X1(["exit 1"])
  B7 --> B8{"sha256 empty or<br/>64 lowercase hex?"}
  B7 -->|"metadata URL<br/>with userinfo"| X5(["exit 5"])
  B8 -->|"no"| X7(["exit 7"])
  B8 -->|"yes"| B9{"download_url present,<br/>absolute http or https?"}
  B9 -->|"no"| X4B(["exit 4"])
  B9 -->|"userinfo or a query"| X5B(["exit 5"])
  B9 -->|"yes"| B10{"its server's origin,<br/>path ends in the artifact?"}
  B10 -->|"no"| X5C(["exit 5"])
  B10 -->|"yes"| B13(["Galaxy entry: download_url<br/>without fragment, sha256"])
```

The version check runs before any metadata request, so a lenient snapshot
entry such as `*` buys no request and never reaches `--frozen`. A Galaxy entry
costs a metadata fetch, never a tarball. Why each `download_url` refusal
exists: [download_url and frozen installs](lockfile-format.md).

## Write, preview or gate

```mermaid
flowchart TD
  W0["path: --lock-file, lock_file,<br/>else galaxy.lock beside"] --> W1{"--check?"}
  W1 -->|"yes"| W2["LoadRequired the file"]
  W1 -->|"no"| W4{"--dry-run?"}
  W2 -->|"absent or invalid"| X6(["exit 6"])
  W2 --> W3["Compare: Would lines,<br/>Check or Dry run summary"]
  W4 -->|"yes"| W5["baseline: the file, else<br/>empty; unreadable warns"]
  W4 -->|"no"| W7["lockfile.Save: canonical<br/>bytes, atomic write"]
  W5 --> W3
  W7 --> W8["print Lockfile written"]
  W7 -->|"filesystem error"| X1(["exit 1"])
  W3 --> W9["saveLockSnapshot: under<br/>--dry-run only a persisted one"]
  W8 --> W10["SaveStore"]
  W9 --> W11["writeRunMetrics,<br/>skipped under --dry-run"]
  W10 --> W11
  W11 --> W12{"--check and<br/>diff not empty?"}
  W12 -->|"yes"| X6D(["exit 6, ErrLockfileDrift,<br/>save failure appended"])
  W12 -->|"no"| W13{"snapshot save failed?"}
  W13 -->|"yes"| XS(["exit 2 or 4<br/>by cause"])
  W13 -->|"no"| X0(["exit 0"])
```

`--check` is read before `--dry-run`, so with both the verdict applies and
`--dry-run` changes only the save and the metrics. `lockCheck` loads with
`LoadRequired`, so a missing file fails as missing, never as the drift
`Compare(nil, lf)` would show. `Lockfile written` prints before the snapshot
save: the file stays valid if the save fails.

## Exits

| Exit | Decided in | Cause |
| --- | --- | --- |
| 1 | `lockfile.Save`; solver | a filesystem error, no snapshot or metrics written; an unparseable `requirements.yml` constraint |
| 1 | `galaxyLockfileEntry` | a version document answering a status other than `404`, which no class claims |
| 2 | `loadRoots`, `buildLockfile` | requirements refused; inexact version; unpinned git, url or role locator |
| 2, 3, 4, 5, 7 | resolution | by cause, as on [install flow](flow-install.md) |
| 3 | `galaxyLockfileEntry` | a `404` for a collection or version the resolve named, often a replayed one: `ErrNoSemverCandidates` |
| 4 | `galaxyLockfileEntry` | metadata unavailable; `download_url` missing or not absolute http(s) |
| 5 | `lockableDownloadURL`, `checkServerArtifactURL`, `normalizeVersionsURL` | userinfo, a query, or not its server's artifact; a metadata URL with userinfo |
| 6 | `lockCheck` | file missing or invalid; drift |
| 7 | `galaxyLockfileEntry` | a malformed sha256, `ErrMalformedArtifactSHA256` |
| 2, 4 | `SaveStore` | the save failed alone; appended to drift otherwise |

## Flags that change the flow

| Flag | Diagram | Effect |
| --- | --- | --- |
| `--check` | Write, preview or gate | compare with the file instead of writing; any difference exits 6 |
| `--dry-run` | Write, preview or gate | diff against the file, write nothing; discovery discards its builds |
| `--refresh` | resolution | vetoes replay, re-advertises git refs, skips url and Galaxy role pins |
| `--offline` | resolution | pins and recorded resolve only, a miss exits 4; no pin or metadata recorded |
| `--no-cache` | resolution | no pin or recorded resolve read; builds kept for the run only |
| `--no-deps` | resolution | solver and role walk stop at the roots; part of the signature |
| `--clear-cache` | Shared setup | forgets metadata and pins, deletes artifacts, keeps the recorded resolve |
| `--metrics-file` | Write, preview or gate | JSON report once the save step is reached |

`--check` still runs the full resolve, `--refresh` included: `initInstall`
never reads it. `lock` mounts no `--frozen`, so the flag is undefined (exit 2) and
`GO_GALAXY_FROZEN` never reaches it; it mounts no signature flag either.
`--s3-bucket` acts in Shared setup. The rest supply values without branching:
paths, pools, servers, `--lock-file` and the other S3 settings.

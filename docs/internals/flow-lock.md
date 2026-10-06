# lock flow

`lock` resolves the requirements file as install does without `--frozen`,
keeping the pins the existing lockfile holds, builds the lockfile in memory
and writes it, previews it under `--dry-run` or compares it under `--check`.
Options:
[`lock`](../reference/cli.md#lock); code meanings: [Exit codes](../reference/exit-codes.md); the
file itself: [Lockfile format](lockfile-format.md).

## Overview

```mermaid
flowchart TD
  L1(["Shared setup: lock held,<br/>snapshot loaded"]) --> L2["loadRoots"]
  L2 -->|"refused"| X2(["exit 2"])
  L2 --> L2R["RecordProject unless<br/>--dry-run, failure warns"]
  L2R --> LF["read the lockfile once, see<br/>Keeping the lockfile's pins"]
  LF --> L3["resolveCollectionsInternal,<br/>keeping its collection pins"]
  L3 -->|"failed"| XR(["exit 1, 2, 3, 4,<br/>5 or 7 by cause"])
  L3 --> L3W["warnUnpublished: a pin no<br/>longer published moved"]
  L3W --> L4["resolveRoles, keeping<br/>each matching role's pin"]
  L4 -->|"failed"| XR
  L4 --> L5["buildLockfile, see<br/>Building the lockfile"]
  L5 -->|"refused"| XB(["exit 2, 3, 4,<br/>5 or 7 by cause"])
  L5 -->|"404 for a kept version,<br/>once (keptVersionGone)"| L5A["resolve again without<br/>the replay, then warn"]
  L5A --> L5
  L5 --> L6["write, preview or gate,<br/>see its diagram"]
  L6 --> XO(["exit 0, 1, 2,<br/>4 or 6 by cause"])
```

`lockWithState` runs under [Shared setup](commands.md#shared-setup) with the
banner `Resolving for lockfile`. Resolution and source discovery are install's,
drawn on [install flow](flow-install.md): the same
`resolveCollectionsInternal` and `resolveRoles`, without `newVerifyContext` or
`planCollections`, plus the lockfile's pins. It installs nothing but saves the
snapshot, so later runs replay the resolve. It records its project once the
file loads, as install does, `--check` included, but leaves the install paths
to install ([Project registry](cache.md#project-registry)).

## Keeping the lockfile's pins

```mermaid
flowchart TD
  K0["readExistingLockfile: --lock-file,<br/>lock_file, else galaxy.lock beside"] --> K1{"lockfile.Load"}
  K1 -->|"absent"| K2(["no pins, no output"])
  K1 -->|"not loadable"| K3{"--check?"}
  K3 -->|"yes"| K4(["no pins, no output;<br/>fails after resolving"])
  K3 -->|"no"| K5(["no pins, one warning"])
  K1 -->|"loaded"| K6{"--refresh without<br/>--offline?"}
  K6 -->|"yes"| K7(["no pins"])
  K6 -->|"no"| K8(["newLockPreferences: Galaxy entries<br/>by fqdn, url by source, git<br/>for matchGitRoots, roles by name"])
```

The file is read after `loadRootsAndRecordProject`, so a refused requirements
file reads nothing, and that one read serves the pins, the `--dry-run`
baseline and the `--check` verdict. `withLockPreferences` puts the index on
`collectionDeps`; only `lockWithState` sets it, so install, warm and outdated
resolve without it. `--no-cache`, `--clear-cache`, `--offline`, `--no-deps` and
`--dry-run` keep the pins, since the lockfile is no cache.

| Reader | Rule |
| --- | --- |
| `MetadataProvider.Preferred` | Answers the version the Galaxy entry pins, from the file alone. A git or url pin, or no Galaxy entry, has no preference |
| `MetadataProvider.Confirm` | Asked once that version passes the accumulation, so one the requirements exclude costs nothing: the root document as `Highest` reads it, so past its 10-minute window it is revalidated and a server that dropped the collection gives way to the next, then the version document under the exact-version policy. Two requests on an empty cache, none within the window, one conditional request past it, and `Dependencies` and `buildLockfile` then read the cache |
| A `404` there | Recorded unpublished without output, since the solver may leave the package out; any other error, `ErrOfflineMode` included, ends the solve |
| `resolveFromSnapshots`, `tryIncrementalResolveWithSnapshot` | A full or incremental replay holding a Galaxy collection at another version than its entry is dropped before it is recorded (`agreesWith`), and the solve runs: the recorded resolution may come from a run that never read the file, such as `install --refresh`. An entry whose root's own constraint excludes the locked version does not count, since that version moved on purpose. The resolution `lock` records holds each kept commit's and sha256's locator, which every command's replay hands only to a run whose own root expanded into it (`sourcesMatchRoots`, [Resolution replay](cache.md#resolution-replay)) |
| `warnUnpublished` | After the resolve, one warning per collection recorded unpublished that resolved to another version; a version the constraints exclude is never confirmed, so it draws none |
| `keptVersionGone`, in `lockWithState` | A replay agreeing with the file keeps a version without asking its server. When `buildLockfile` then meets a `404` for it, as once `--clear-cache` or 30 days dropped its metadata, the collections resolve once more without the replay, so `Confirm` finds the pin gone and the warning above follows |
| `lockPreferences.gitRoots`, once per `expandGitRoots` | Matches the git roots to the git entries by `matchGitRoots`, the rule `--frozen` judges them by, over every root at once, since which entries a root owns depends on the roots beside it. A root keeps the commit every entry it owns shares. One asked at a commit keeps none, nor does one owning no entry, as after a ref change, or entries at two commits; each resolves as install does, with no warning |
| `expandLockedGitRoot` | A recorded pin at the locked commit replays, one at another commit never; else `--offline` fails; else `acquireGitRoot` fetches that commit, its pin recorded as `lockedPinPolicy` allows. `ErrGitCommitNotFound` there warns once, naming the root's one collection or else its source and ref, and the root resolves as if unlocked |
| `lockedPinPolicy`, before a locked commit is fetched | One advertisement, no pack, bounded by `GitDeadline` as a fetch is: the pin is recorded only while the ref names that commit and, for a role, while `roleVersionFor` makes the locked label of the ref name advertised, since a pin the file alone decided would reach every project on a shared cache. A ref the remote no longer has records none; any other advertisement failure ends the run as the fetch's would, an expired deadline as `ErrArtifactDownloadDeadline`. A debug line names why no pin is recorded: the ref not advertised, at another commit, or at that commit under another version. A policy that writes nothing, as under `--no-cache`, advertises nothing. So a moved ref's locked commit is fetched by every run, its artifact still committed under its locator |
| `releaseRuledOutGitRoots`, in `resolveCollectionsInternal` | A resolve failing on a collection two roots expand into (`duplicateRootError`), or on a solver conflict whose proof names a collection a kept git root holds (`ConflictError.Packages`), releases each such root: one warning, then the resolve runs again from forgotten discoveries, the root as if unlocked but never replaying a recorded pin at the released commit. A failure naming no kept root ends the run as without the file |
| `lockPreferences.urlEntry`, `expandLockedURLRoot` | The entry locked from the root's URL, unless the root asserts another `version:`, the rule `--frozen` accepts a url root by. A recorded pin of the locked sha256 replays, one of other bytes never; else `--offline` fails; else the URL is downloaded, and other bytes than the locked ones warn once and are kept |
| `lockPreferences.role` | Every role request, a root or a dependency, finds its entry by install name under the rule `--frozen` accepts a root by (`verifyRoleRootAgainstLockfile`); a git role asked at a commit is not looked up, as that ref is its pin. A request no entry matches resolves as install does, with no warning |
| `resolveLockedGitRole`, for a git role and a usable Galaxy role | A recorded pin at the locked commit and label whose artifact is cached replays; else `--offline` fails; else `acquireRole` fetches that commit under the locked label, its pin recorded as `lockedPinPolicy` allows. `ErrGitCommitNotFound` there warns once and resolves the role as if unlocked |
| `resolveLockedGalaxyRole` | Asks no v1 API when the entry's server is one `lookupGalaxyRole` asks (`lockedRoleServerAsked`) and its repository and ref pass `galaxyPinResolution`; otherwise one warning, and the role resolves as if unlocked, from a recorded Galaxy pin, else the v1 API. It records no Galaxy pin from the entry, since no v1 answer named that repository this run, and leaves a recorded one as it is |
| A Galaxy role's repository that cannot be fetched | `unreachableRepository`: the role asks the v1 API past a recorded pin or a cached answer naming that repository. Another repository warns once and is taken; the same one fails the run with the fetch's error |
| `resolveLockedURLRole` | A recorded pin of the locked sha256 whose artifact is cached under the request's label replays; else `--offline` fails; else the URL is downloaded, and other bytes warn once and are kept. The label is the request's, as for install, so a `version:` dropped from the requirement relabels the locked bytes and both commands record one pin |

The solver picks a passing preference before any unpreferred package
([The loop](solver.md#the-loop)), so the pins a valid resolution allows are
kept, except one reached only through an entry that resolves anew: that entry
takes its highest allowed version, and what it alone reaches can move to what
that version needs. A collection's server binding is unchanged: a root's
`source:`, else the first server in list order that has the collection; the
entry's own `source` is not read. A kept git or url collection or role takes
its dependencies from its replayed pin or its fetch at the locked commit or
bytes, never from the entry's `deps`.

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
  B6 -->|"refused"| XG(["exit 3, 4,<br/>5 or 7 by cause"])
  B6 --> B11{"collections left?"}
  B4 --> B11
  B5 --> B11
  B11 -->|"yes"| B2
  B11 -->|"no"| B12(["SchemaVersionFor<br/>picks the schema"])
```

A Galaxy entry, in `galaxyLockfileEntry`:

```mermaid
flowchart TD
  B7["loadCollectionMetadata"] -->|"any status but 404,<br/>unreachable, bad document"| X4(["exit 4"])
  B7 -->|"404: gone<br/>from its server"| X3(["exit 3"])
  B7 --> B8{"sha256 empty or<br/>64 lowercase hex?"}
  B7 -->|"metadata URL<br/>with userinfo"| X5(["exit 5"])
  B8 -->|"no"| X7(["exit 7"])
  B8 -->|"yes"| B9{"download_url present,<br/>absolute http or https?"}
  B9 -->|"no"| X4B(["exit 4"])
  B9 -->|"userinfo or a query"| X5B(["exit 5"])
  B9 -->|"yes"| B10{"on its server's<br/>origin?"}
  B10 -->|"no"| X5C(["exit 5"])
  B10 -->|"yes"| B13(["Galaxy entry: download_url<br/>without fragment, sha256"])
```

The version check runs before any metadata request, so a lenient snapshot
entry such as `*` buys no request and never reaches `--frozen`. A Galaxy entry
costs a metadata fetch, never a tarball. Why each `download_url` refusal
exists: [Loading the lockfile](boundaries.md#loading-the-lockfile).

## Write, preview or gate

```mermaid
flowchart TD
  W0["the lockfile read<br/>before the resolve"] --> W1{"--check?"}
  W1 -->|"yes"| W2{"read failed?"}
  W1 -->|"no"| W4{"--dry-run?"}
  W2 -->|"absent or invalid"| X6(["exit 6"])
  W2 -->|"no"| W3["Compare: Would lines,<br/>Check or Dry run summary"]
  W4 -->|"yes"| W5["baseline: the file read,<br/>else empty"]
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
`--dry-run` changes only the save and the metrics. `lockCheck` returns a failed
read as `lockfile.RequiredError` maps it, the error `LoadRequired` gives, so a
missing file fails as missing, never as the drift `Compare(nil, lf)` would
show. `Lockfile written` prints before the snapshot save: the file stays valid
if the save fails.

## Exits

| Exit | Decided in | Cause |
| --- | --- | --- |
| 1, 2, 4, 8, 9 | [Shared setup](commands.md#shared-setup) | configuration, backend open, lock or snapshot load |
| 2 | `loadRoots` | requirements file missing, unreadable or invalid, or a Galaxy `source:` naming no server of the run |
| 1, 2, 3, 4, 5, 7 | resolution | by cause, as on [install flow](flow-install.md#exits) |
| 4 | `MetadataProvider.Confirm` | under `--offline`, a locked Galaxy version the requirements allow whose root or version document is not cached: `ErrOfflineMode` |
| 4 | `expandLockedGitRoot`, `expandLockedURLRoot` | under `--offline`, a locked git commit or url sha256 with no recorded pin: `ErrOfflineMode` |
| 4 | `resolveLockedGitRole`, `resolveLockedURLRole` | under `--offline`, a locked role commit or sha256 with no recorded pin and cached artifact: `ErrOfflineMode` |
| 2 | `buildLockfile` | inexact version; unpinned git, url or role locator |
| 3 | `galaxyLockfileEntry` | a `404` for a collection or version the resolve named, often a replayed one the lockfile does not pin (a pinned one resolves again first): `ErrNoSemverCandidates` |
| 4 | `galaxyLockfileEntry` | any status but `404` (`ErrGalaxyAuthFailed`, `ErrGalaxyServerUnavailable`), an unreachable server, a document not JSON or of the wrong shape, a bad timestamp included |
| 5 | `normalizeVersionsURL` | a metadata URL with userinfo |
| 7 | `galaxyLockfileEntry` | a malformed sha256, `ErrMalformedArtifactSHA256` |
| 4 | `lockableDownloadURL` | `download_url` missing or not absolute http(s) |
| 5 | `lockableDownloadURL` | `download_url` with userinfo or a query |
| 5 | `checkDownloadURLOrigin` | `download_url` off its server's origin |
| 1 | `lockfile.Save` | a filesystem error; no snapshot or metrics written |
| 6 | `lockCheck` | file missing or invalid; drift |
| 2, 4 | `SaveStore` | the save failed alone; appended to drift otherwise |

## Flags that change the flow

| Flag | Diagram | Effect |
| --- | --- | --- |
| `--check` | Write, preview or gate | compare with the file instead of writing; any difference exits 6 |
| `--dry-run` | Write, preview or gate | diff against the file, write nothing; discovery discards its builds |
| `--refresh` | Keeping the lockfile's pins | sets them aside unless `--offline`; otherwise as for install, on [install flow](flow-install.md#flags-that-change-the-flow) |
| `--no-deps` | Overview | as for install; a root still keeps its pin |
| `--offline` | Overview | recorded pins and resolve only, a miss exits 4, a locked version whose documents are not cached or a locked commit, sha256 or role pin not recorded included; no pin or metadata recorded |
| `--no-cache` | Overview | no recorded pin or resolve read, the lockfile's pins still kept; builds kept for the run only |
| `--clear-cache` | [Shared setup](commands.md#shared-setup) | forgets metadata and pins, deletes artifacts, keeps the recorded resolve; skipped under `--dry-run` |
| `--s3-bucket` | [Shared setup](commands.md#shared-setup) | S3 backend, so a lock lost mid-run is possible |
| `--metrics-file` | Write, preview or gate | JSON report once the save step is reached |

`--check` still runs the full resolve and honors `--refresh`: `initInstall`
never reads `--check`. The other options supply values without changing the
flow: the pool sizes `--workers` and `--download-workers`, `--cache-dir`,
`--download-path` and `--roles-path` (unused, since only install records
them), the other paths and files, `--lock-file`, the server and output
options, and the other `--s3-*` flags.

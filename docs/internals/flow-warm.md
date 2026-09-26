# warm flow

`warm` resolves like `install`, then fills the artifact cache and the extracted
store for every collection and role without writing a collections or roles
tree. Options: [`warm`](../reference/cli.md#warm); code meanings:
[Exit codes](../reference/exit-codes.md).

## Overview

```mermaid
flowchart TD
    S(["go-galaxy warm"]) --> CF["BuildCollectionConfig,<br/>clients"]
    CF -->|"refused"| X2(["exit 2"])
    CF --> NC{"--no-cache set?"}
    NC -->|"yes"| X2
    NC -->|"no"| SU["shared setup: open, lock,<br/>load snapshot, record project"]
    SU -->|"fails"| XS(["exit 1, 2, 4, 8<br/>or 9 by cause"])
    SU --> PL["plan: roots, verification,<br/>collections, then roles"]
    PL -->|"fails"| XP(["exit 2, 3, 4, 5,<br/>6 or 7 by cause"])
    PL --> D{"--dry-run set?"}
    D -->|"yes"| PV["preview: probe, report,<br/>save a persisted snapshot"]
    PV --> XD(["exit 0, 5 or 7,<br/>or the save error's code"])
    D -->|"no"| WC["warmCollections"]
    WC --> A{"every collection warmed?"}
    A -->|"yes"| WR["warmRoles"]
    A -->|"no"| SV
    WR --> SV["SaveStore, then writeRunMetrics"]
    SV --> F{"any item failed?"}
    F -->|"yes"| XF(["exit 5, 7 or 10<br/>by cause"])
    F -->|"no, save failed"| XSV(["exit 2, 4 or 8<br/>by cause"])
    F -->|"no"| X0(["exit 0"])
```

`runWarm` refuses `--no-cache` (`ErrWarmCacheDisabled`) before
`cacheBackend.New`, so no backend opens and no lock is taken. Everything else
runs inside `withBackend`, whose startup is
[Shared setup](commands.md#shared-setup): the dead-run temp sweep,
`--clear-cache` and `RecordProject` happen there.

## Plan

```mermaid
flowchart TD
    L["loadRoots: requirements file"] -->|"fails"| X2(["exit 2"])
    L --> V["newVerifyContext"]
    V -->|"fails"| X2
    V --> F{"--frozen set?"}
    F -->|"yes"| RL["resolveFromLockfile"]
    F -->|"no"| RS["resolveCollectionsInternal,<br/>as install does"]
    RL -->|"fails"| X6(["exit 6"])
    RL --> PC["planCollections: map,<br/>roots present, levels"]
    RS --> PC
    RS -->|"fails"| XR(["exit 2, 3, 4,<br/>5 or 7 by cause"])
    PC -->|"fails"| XP(["exit 2 or 3<br/>by cause"])
    PC --> F2{"--frozen set?"}
    F2 -->|"yes"| RR["resolveRolesFromLockfile"]
    F2 -->|"no"| RD["resolveRoles"]
    RR -->|"fails"| X6R(["exit 6"])
    RD -->|"fails"| XRR(["exit 2, 3, 4,<br/>5 or 7 by cause"])
    RR --> OUT(["to warming or the preview"])
    RD --> OUT
```

Resolution, discovery and replay are install's, drawn on
[install flow](flow-install.md). One order differs: `warm` runs
`planCollections` before any role resolves, so a cycle or a dropped root
fails before a role repository is fetched. The install levels are computed
and discarded.

## Warming collections and roles

```mermaid
flowchart TD
    P["startPrefetcher: probe,<br/>prefetch cache misses"] --> L{"next collection,<br/>run not canceled?"}
    L -->|"yes"| W["warmOne on the<br/>--workers pool"]
    W -->|"ok"| OK1["print Cached"]
    W -->|"error"| F1["print Failed,<br/>record cause"]
    OK1 --> L
    F1 --> L
    L -->|"no"| J["wait for workers,<br/>close the prefetcher"]
    J --> A{"any collection failed?"}
    A -->|"yes"| S(["back to SaveStore"])
    A -->|"no"| RO(["to the roles"])
```

The prefetcher probes the cache and downloads the misses in background; it
is install's with a nil root and nil levels
([Prefetch and handoff](install-pipeline.md#prefetch-and-handoff)). Roles
follow only when every collection warmed:

```mermaid
flowchart TD
    R{"next role,<br/>run not canceled?"} -->|"yes"| RF["fetchRoleArtifact: cache<br/>hit, else refetch the pin"]
    RF -->|"error, or offline miss"| F2["print Failed,<br/>record cause"]
    RF --> RE["extractStore.Ensure, then<br/>SetWarmed role:name@version"]
    RE -->|"error"| F2
    RE -->|"ok"| OK2["print Cached: role"]
    F2 --> R
    OK2 --> R
    R -->|"no"| S(["back to SaveStore"])
```

A per-item failure never stops the others; the causes join behind the
`ErrInstallationFailed` headline built by `warmError`.

## One collection

```mermaid
flowchart TD
    H["prefetch.Wait: take the handoff,<br/>warn if the prefetch failed"] --> P["prepareInstall: prefetched<br/>bytes, cache hit or download"]
    P -->|"error"| E1{"canRetryCacheHit,<br/>sha256 mismatch?"}
    P --> V["verifyPinnedSHA, then<br/>verifyCollectionSignatures"]
    V --> EN["extractStore.Ensure<br/>under the sha256"]
    EN --> REC["recordWarmed: SetWarmed<br/>ns.name@version, stamped now"]
    REC --> OK(["Cached"])
    V -->|"error"| E2{"canRetryCacheHit,<br/>repairable by a refetch?"}
    EN -->|"error"| E2
    E1 -->|"yes: evict"| EV["force one refetch"]
    E2 -->|"yes: evict unless a<br/>locked download_url"| EV
    EV --> P
    E1 -->|"no"| FL(["Failed, cause recorded"])
    E2 -->|"no"| FL
```

`canRetryCacheHit` holds for a cache hit on the first try, not under
`--offline`, and both decisions need it. `warmOne` is `prepareWithRecovery`
with `warmVerifyAndEnsure` as its action, so acquisition and the refetch-once
rule are install's:
[Acquiring an artifact](install-pipeline.md#acquiring-an-artifact) and
[Bounded recovery](install-pipeline.md#bounded-recovery). `recordWarmed` stamps
cache hits too, which is what keeps a tree through `cleanup` for 30 days
(`WarmedEntryMaxAge`); install must never call it.

## The dry-run preview

```mermaid
flowchart TD
    W["WarmedArtifactSHAByKey:<br/>entries warmed within 30 days"] --> PR["warmDryRunProbe per collection,<br/>parallel on --workers"]
    PR --> C{"artifact cached?<br/>one Meta lookup"}
    C -->|"no"| V["verdict"]
    C -->|"yes"| PIN{"--offline, and recorded digest<br/>contradicts the pin?"}
    PIN -->|"yes: would fail"| V
    PIN -->|"no"| RD{"extractStore.Ready under the pin,<br/>else the warmed sha?"}
    RD -->|"yes: Already warm"| V
    RD -->|"no: Would warm"| V
    V --> REP["print verdicts in key order,<br/>then the summary line"]
    REP --> RL["roles: warmRoleDryRunProbe,<br/>then the roles summary"]
    RL --> SV["saveDryRunSnapshotIfPersisted"]
    SV --> F{"any would fail?"}
    F -->|"yes"| XF(["exit 7 for a digest mismatch,<br/>else 5"])
    F -->|"no"| X0(["exit 0, or the<br/>save error's code"])
```

The preview downloads, extracts and stamps nothing; an uncached item under
`--offline` is a would-fail. A git, url or role source with no usable pin was
still fetched during resolution and its build discarded
([Dry run](install-pipeline.md#dry-run)).

## Exits

| Exit | Decided in | Cause |
| --- | --- | --- |
| 2 | `runWarm` | `--no-cache`, before any backend |
| 1, 2, 4, 8, 9 | `BuildCollectionConfig`, `initInstall` | [Shared setup](commands.md#shared-setup) |
| 2 | `loadRoots`, `newVerifyContext`, `buildCollectionsMap` | requirements unreadable or invalid, keyring or signature config, unsafe resolved name, inexact version or duplicate key |
| 3 | solver, `planCollections` | conflict, no candidate, dropped root, cycle |
| 6 | `resolveFromLockfile`, `resolveRolesFromLockfile` | `--frozen`: lockfile missing, invalid, not covering the roots |
| 1, 2, 3, 4, 5, 7 | resolution | by cause, as on [install flow](flow-install.md) |
| 7 | `warmError` | a cause is a sha256, commit or identity mismatch |
| 10 | `warmError` | a cause is a signature verdict |
| 5 | `warmError` | any other item failure, a network cause included |
| 2, 4, 8 | `SaveStore` | save failed and no item did |

## Flags that change the flow

| Flag | Diagram | Effect |
| --- | --- | --- |
| `--no-cache` | Overview | exit 2 before open |
| `--frozen` | Plan | lockfile instead of solver and role discovery |
| `--dry-run` | Overview | preview replaces warming; no `--clear-cache`, registry record or metrics |
| `--offline` | Plan, Warming collections and roles, One collection, The dry-run preview | resolves as install does; a cache miss fails the item; no eviction |
| `--refresh`, `--no-deps` | Plan | as for install |
| `--clear-cache` | [Shared setup](commands.md#shared-setup) | drops caches and artifact files before planning |
| `--keyring`, `--disable-gpg-verify` | Plan, One collection | verification on: signatures checked before `Ensure` |
| `--s3-bucket` | [Shared setup](commands.md#shared-setup) | S3 backend, so a lock lost mid-run is possible |
| `--metrics-file` | Overview | report after the save |

Accepted without changing the flow: `--workers` and `--download-workers` (pool
sizes), `--download-path` and `--roles-path` (only recorded in the registry),
`--lock-file`, the other signature and `--s3-*` flags, and the output and
server options.

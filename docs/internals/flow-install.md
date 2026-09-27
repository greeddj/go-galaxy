# install flow

`install` resolves the requirements file, or reads the lockfile under
`--frozen`, then installs collections level by level and roles after them.
Options: [Options](../reference/cli.md#options); codes: [Exit codes](../reference/exit-codes.md);
mechanism: [Install pipeline](install-pipeline.md).

## Overview

```mermaid
flowchart TD
  O1(["Shared setup: lock held,<br/>snapshot loaded"]) --> O2["openCollectionsRoot<br/>at --download-path"]
  O2 -->|"symlink in the way"| OX5(["exit 5"])
  O2 -->|"empty path, other failure"| OX1(["exit 1"])
  O2 --> O3["prepareInstallPlan,<br/>see Planning"]
  O3 -->|"refused"| OXP(["exit 1 to 7<br/>by cause"])
  O3 --> O4["openRolesRootIfNeeded"]
  O4 -->|"cannot open"| OX1
  O4 --> O5{"--dry-run?"}
  O5 -->|"yes"| O6["installDryRun,<br/>see Dry run"]
  O5 -->|"no"| O7["installLevels, see<br/>Collection install"]
  O7 -->|"ErrMissingCollection"| OX2(["exit 2"])
  O7 --> O8{"any collection failed?"}
  O8 -->|"no"| O9["installRoles, see Roles"]
  O8 -->|"yes: roles not attempted"| O10["report unverified skips,<br/>finalizeInstall: SaveStore"]
  O9 --> O10
  O10 --> O11["writeRunMetrics"]
  O11 --> O12{"failures or<br/>save error?"}
  O6 --> O12
  O12 -->|"an item failed"| OXF(["exit 7 or 10 by cause,<br/>else 5"])
  O12 -->|"only the save"| OXS(["exit 2 or 4<br/>by cause"])
  O12 -->|"neither"| OX0(["exit 0"])
```

`installWithState` runs under [Shared setup](commands.md#shared-setup). Roles
wait for every collection level, so a failed run neither reports unattempted
roles nor installs them against a half-done tree. `plan.prefetch.Close` is
deferred there, so every prefetch worker joins before the lock is released.

## Planning

```mermaid
flowchart TD
  P1["loadRoots: read and<br/>parse the requirements"] -->|"missing, unreadable,<br/>invalid"| PX2(["exit 2"])
  P1 --> P2["newVerifyContext: keyring,<br/>policy, signatures: blocks"]
  P2 -->|"refused, or signatures:<br/>without a keyring"| PX2
  P2 --> P3{"--frozen?"}
  P3 -->|"yes"| P4["LoadRequired lockfile,<br/>resolveFromLockfile"]
  P4 -->|"refused"| PX6(["exit 6"])
  P4 --> P6["resolveRolesFromLockfile"]
  P6 -->|"a roles: entry<br/>unlocked"| PX6
  P3 -->|"no"| P5["resolveCollectionsInternal,<br/>see Collection resolution"]
  P5 -->|"failed"| PXR(["exit 1, 2, 3, 4,<br/>5 or 7 by cause"])
  P5 --> P7["resolveRoles,<br/>see Roles"]
  P7 -->|"failed"| PXR
  P6 --> P8["planCollections"]
  P7 --> P8
  P8 -->|"unsafe name, inexact<br/>version, duplicate key"| PX2B(["exit 2"])
  P8 -->|"root unresolved,<br/>cycle"| PX3(["exit 3"])
  P8 --> P9{"--no-cache or<br/>--dry-run?"}
  P9 -->|"no"| P10["startPrefetcher on<br/>--download-workers"]
  P9 -->|"yes: no prefetcher"| P11(["plan ready"])
  P10 --> P11
```

Why this order: [Plan construction](install-pipeline.md#plan-construction).

## Collection resolution

```mermaid
flowchart TD
  R1["expandSourceRoots: git,<br/>then url, see Source discovery"] -->|"failed"| RXD(["exit 1, 2, 3, 4,<br/>5 or 7 by cause"])
  R1 --> R2["requirements signature:<br/>roots, --no-deps, servers"]
  R1 -->|"two roots,<br/>one collection"| RX2(["exit 2"])
  R2 --> R3{"--refresh or --no-cache<br/>without --offline?"}
  R3 -->|"no"| R4{"snapshot signature equal,<br/>every root satisfied?"}
  R4 -->|"yes"| R5["replay the snapshot,<br/>no metadata request"]
  R5 --> R11A(["resolved set and graph"])
  R4 -->|"no"| R6{"only some roots changed,<br/>same mode and servers?"}
  R6 -->|"no"| R8["prewarmRootMetadata<br/>on --workers"]
  R6 -->|"yes"| R7["re-solve changed roots,<br/>merge preserved subgraph"]
  R7 -->|"merge unusable"| R8
  R7 -->|"solve failed"| RXD2(["exit 1, 2, 3, 4,<br/>5 or 7 by cause"])
  R7 -->|"merged graph valid"| R10
  R3 -->|"yes"| R8
  R8 --> R9["solveCollections over<br/>the server list"]
  R9 -->|"conflict, no candidate<br/>or a 404"| RX3(["exit 3"])
  R9 -->|"any status but 404, bad<br/>document, unreachable, offline miss"| RX4(["exit 4"])
  R9 -->|"metadata URL<br/>with userinfo"| RX5(["exit 5"])
  R9 --> R10["record the resolution<br/>in the snapshot"]
  R10 --> R11(["resolved set and graph"])
```

`resolveCollectionsInternal` is shared by install, warm and lock. A cycle is
refused later, in `planCollections`, which `lock` never runs. Why roots
expand before the signature: [Resolution replay](cache.md#resolution-replay).
Why the prewarm sits below the replay: [The Provider seam](solver.md#the-provider-seam).

## Source discovery

Each git or url source, root or role, and each Galaxy role once mapped, takes
one path to an exact pin:

```mermaid
flowchart TD
  P1{"pin readable?"} -->|"no"| P3{"--offline?"}
  P1 -->|"yes"| P2["replay the pin, validated<br/>like a remote answer"]
  P2 -->|"invalid pin"| X2(["exit 2"])
  P2 -->|"asserted version differs,<br/>bad dependency key"| X3(["exit 3"])
  P2 -->|"a role whose artifact<br/>is not cached"| P3
  P3 -->|"yes"| X4(["exit 4"])
  P3 -->|"no"| P4{"--refresh on a git<br/>branch or tag pin?"}
  P4 -->|"no"| P6["git: fetch, build; url:<br/>download, read or repack"]
  P4 -->|"yes"| P5["advertise the ref once"]
  P5 -->|"moved"| P6
  P5 -->|"failed"| XF1(["exit 2, 3, 4,<br/>5 or 7 by cause"])
  P5 -->|"same commit,<br/>artifacts cached"| P2
  P6 -->|"failed"| XF(["exit 2, 3, 4,<br/>5 or 7 by cause"])
  P6 --> P7["commit to artifact cache;<br/>--no-cache hands it on"]
  P7 -->|"commit failed"| XF
  P7 --> P8["record the pin unless<br/>--no-cache or --offline"]
  P8 --> OUT(["exact-pin locator:<br/>commit or sha256"])
  P2 --> OUT
```

This runs once per unpinned git or url root and once per role, on
`--download-workers`; mechanism on [Git discovery](install-pipeline.md#git-discovery).
A pin is readable when `cache.PolicyForConstraint` allows: always under
`--offline`, never under `--no-cache`, and under `--refresh` only for a git
commit. `--dry-run` discards the build instead of committing it.

A Galaxy role first maps to a repository and tag, from its pin or from the
servers' v1 role API, then takes the path above:

```mermaid
flowchart TD
  G1{"Galaxy pin readable?"} -->|"yes"| G3(["repository and tag,<br/>then the git path"])
  G1 -->|"no, --offline"| GX4(["exit 4"])
  G1 -->|"no"| G2["ask each server's<br/>v1 role API in order"]
  G2 -->|"no server<br/>serves v1"| GX2(["exit 2"])
  G2 -->|"role or version<br/>unknown"| GX3(["exit 3"])
  G2 -->|"request failed"| GXF(["exit 2, 3, 4,<br/>5 or 7 by cause"])
  G2 --> G3
```

## Collection install

```mermaid
flowchart TD
  C1["next level: a worker per<br/>collection, --workers"] -->|"plan names<br/>a missing key"| CX2(["exit 2"])
  C1 --> CI["each collection installs,<br/>skips or fails, see below"]
  CI --> CL{"level joined:<br/>any failure?"}
  CL -->|"no, levels remain"| C1
  CL -->|"yes, or done"| CE(["back to Overview"])
```

Each collection of the level, on its own worker:

```mermaid
flowchart TD
  C2{"identity safe as<br/>path elements?"} -->|"no"| CF(["record the failure,<br/>print Failed"])
  C2 -->|"yes"| C3{"install record, extract marker<br/>and GALAXY.yml identity agree?"}
  C3 -->|"yes"| C4(["skip; rewrite a drifted GALAXY.yml,<br/>count as unverified"])
  C3 -->|"no"| C5["prepareWithRecovery: acquire,<br/>pin, signatures, extract; one refetch"]
  C5 -->|"failed"| CF
  C5 --> C9(["write GALAXY.yml,<br/>record the install"])
```

Mechanism: [Acquiring an artifact](install-pipeline.md#acquiring-an-artifact),
[Bounded recovery](install-pipeline.md#bounded-recovery) for the one refetch,
and [The extract-done marker](install-pipeline.md#the-extract-done-marker) for
the marker and its tally. On cancellation `runInstallLevel` stops
dispatching and returns nil, so the snapshot still saves and `main` sets the
signal code.

## Roles

| Step | Code | Refusal, exit |
| --- | --- | --- |
| Walk the `roles:` entries level by level, first request wins a name | `resolveRoles`, `dedupeRoleLevel` | a differing later request only warns |
| Resolve each role of a level | `resolveRoleLevel` | see Source discovery |
| Cap the graph | `helpers.RoleGraphMaxRoles` | `ErrInvalidRoleEntry`, exit 2 |
| Queue meta dependencies | `roleDependencies` | unparseable entry, exit 2; a local role is skipped, a collection's role warns |
| Install, `--workers` at a time | `installRoles` | foreign directory, fetch or extract failure: joined as a Failed item |

Mechanism: [Role resolution](install-pipeline.md#role-resolution) and
[Installing roles](install-pipeline.md#installing-roles).

## Dry run

`installDryRun` replaces both install phases with read-only probes that report
`Up to date`, `Would install` or `Would fail`; a would-fail exits as the real
failure would. The snapshot saves only if one already existed. Details:
[Dry run](install-pipeline.md#dry-run).

## Exits

| Exit | Decided in | Cause |
| --- | --- | --- |
| 1, 2, 4, 8, 9 | [Shared setup](commands.md#shared-setup) | configuration, backend open, lock or snapshot load |
| 5 | `openCollectionsRoot` | a symlinked `ansible_collections` |
| 1 | `openCollectionsRoot` | an empty path or another OS error |
| 2 | `loadRoots` | requirements file missing, unreadable or invalid |
| 2 | `newVerifyContext` | keyring or signature config |
| 6 | `--frozen` planning | lockfile missing, invalid, not covering a root, `download_url` off its server |
| 2 | discovery | two roots for one collection; an invalid pin; a git repository that lacks the `name:` asked for, holds no collection or one declared twice, or has a `galaxy.yml` that is invalid or names an inexact version; no v1 API, or a v1 record refused |
| 3 | discovery | unknown ref, role or version; a found role's versions `404`; url version mismatch |
| 4 | discovery | git or url transport, any v1 API status but `404`, an `--offline` miss |
| 5 | discovery | a git tree or url tarball that is no artifact |
| 7 | discovery | a git remote that did not ship the advertised commit intact |
| 1 | solver | an unparseable `requirements.yml` constraint |
| 3 | solver | conflict, no candidate (any `404` the solver meets included), bad dependency key |
| 4 | solver | any Galaxy status but `404` (auth or unavailable), an unreachable server, a document not JSON or of the wrong shape (a bad timestamp included), an `--offline` miss |
| 5 | solver | a metadata URL with userinfo |
| 2 | `planCollections` | unsafe resolved name, inexact version, duplicate key |
| 3 | `planCollections` | root unresolved, cycle |
| 1 | `openRolesRoot` | an empty path or another OS error |
| 2 | `installLevels` | the plan names a missing key (`ErrMissingCollection`) |
| 5, 7, 10 | `installError` | failed items behind `ErrInstallationFailed`; 7 or 10 when a cause is one |
| 2, 4 | `finalizeInstall` | the snapshot save failed alone; otherwise appended to the item failure |

## Flags that change the flow

| Flag | Diagram | Effect |
| --- | --- | --- |
| `--frozen` | Planning | lockfile replaces discovery and solver; sha256 pins checked before extraction |
| `--dry-run` | Overview, Planning | no roots created, no prefetcher, builds discarded; preview, metrics skipped |
| `--offline` | Source discovery, Collection install | pins and cache only; a miss exits 4 or fails the item |
| `--refresh` | Collection resolution, Source discovery | vetoes replay, re-advertises refs, re-asks v1, re-downloads url sources |
| `--no-cache` | Collection resolution, Source discovery, Planning | no artifact cache, extracted store or prefetcher; builds go straight to install |
| `--no-deps` | Collection resolution, Roles | part of the signature; solver and role walk stop at the roots |
| `--keyring` | Planning, Collection install | verifies before extraction; a cache hit still loads metadata; skips count unverified |
| `--clear-cache` | [Shared setup](commands.md#shared-setup) | forgets metadata and pins, deletes artifacts, keeps the recorded resolve; skipped under `--dry-run` |
| `--s3-bucket` | [Shared setup](commands.md#shared-setup) | S3 backend, so a lock lost mid-run is possible |
| `--metrics-file` | Overview | JSON report after a real run |

`--disable-gpg-verify` turns `--keyring` off. The other options supply values
without changing the flow: the pool sizes `--workers` and
`--download-workers`, `--cache-dir`, the other paths and files,
`--lock-file`, the server and output options, and the other signature and
`--s3-*` flags.

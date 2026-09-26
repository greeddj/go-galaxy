# Command flows

These pages draw each command's control flow and the exit code at every place
it can stop. Option meanings are on the [CLI reference](../reference/cli.md), code
meanings on [Exit codes](../reference/exit-codes.md).

## How to read the diagrams

A rectangle is a step, a diamond a decision with its answers on the arrows,
and a rounded node an end: an exit code, or a hand-over to another diagram. An
exit marked "by cause" takes the class of the error that ended the run, in the
order of [Exit code classes](http-output-exit-codes.md#exit-code-classes); the
page's Exits table resolves it.

Two outcomes hold for every command and are not drawn again. A caught SIGHUP,
SIGINT or SIGTERM exits 129, 130 or 143 and outranks any other exit. Once the
cache lock is held, losing it mid-run turns any result into exit 8.

## Commands

| Command | Page | Does |
| --- | --- | --- |
| `install` | [install flow](flow-install.md) | Resolves, or reads the lockfile under `--frozen`, then installs collections and roles |
| `lock` | [lock flow](flow-lock.md) | Resolves and writes `galaxy.lock`, or gates on drift under `--check` |
| `warm` | [warm flow](flow-warm.md) | Fills the artifact cache and extracted store, installs nothing |
| `cleanup` | [cleanup flow](flow-cleanup.md) | Removes what no recorded project reaches |
| `outdated` | [outdated flow](flow-outdated.md) | Compares locked or installed versions with their sources, no cache |
| `hash`, `tree`, `explain` | [hash, tree and explain flows](flow-hash-tree-explain.md) | Read the lockfile and requirements file from disk |

## Process entry and exit

```mermaid
flowchart TD
  M1["main calls run"] --> M2["newRootCommand: install default,<br/>NoArguments, errRecorder"]
  M2 --> M3["signal.Notify: SIGINT,<br/>SIGTERM, SIGHUP"]
  M3 --> M4["app.Run, see<br/>Command dispatch"]
  M4 -.->|"signal arrives"| M5["record it, cancel<br/>the run context"]
  M5 -.->|"command unwinds"| M6
  M4 --> M6["handleResult"]
  M6 --> M7{"signal caught?"}
  M7 -->|"yes"| X1(["exit 128 + signal:<br/>129, 130 or 143"])
  M7 -->|"no"| M8{"an error?"}
  M8 -->|"no"| X0(["exit 0"])
  M8 -->|"captured by ExitErrHandler"| X2(["FromError class,<br/>error printed"])
  M8 -->|"bare: a refused flag"| X3(["exit 2, printed<br/>unless urfave did"])
```

SIGQUIT stays uncaught, so Go's goroutine dump still works. How
`handleResult` and `errRecorder` decide is under
[Exit code classes](http-output-exit-codes.md#exit-code-classes).

## Command dispatch

```mermaid
flowchart TD
  D1["urfave parses the root flags"] --> D2{"root flags and<br/>their variables parse?"}
  D2 -->|"no"| XU(["exit 2"])
  D2 -->|"--help or --version"| XH(["print it, exit 0"])
  D2 -->|"yes"| D3{"first positional word?"}
  D3 -->|"a command or alias"| D4["that command,<br/>the words after it"]
  D3 -->|"none"| D5["install, the<br/>DefaultCommand"]
  D3 -->|"anything else"| D6["install with<br/>those words"]
  D4 --> D7{"command flags and<br/>their variables parse?"}
  D5 --> D7
  D6 --> D7
  D7 -->|"--help or -h"| XH2(["command help, exit 0"])
  D7 -->|"no"| XU
  D7 -->|"yes"| D8{"arguments valid?<br/>explain one, others none"}
  D8 -->|"no"| XA(["exit 2, ErrMissingArgument<br/>or ErrUnexpectedArguments"])
  D8 -->|"yes"| D9{"which command?"}
  D9 -->|"install, warm, lock,<br/>outdated, cleanup"| RC(["runCollectionCommand,<br/>see Shared setup"])
  D9 -->|"hash, tree, explain"| FI(["read files from disk:<br/>no Config, no cache"])
```

`commands.NoArguments` is the root's `ArgValidator` because install is the
`DefaultCommand`: without it, a mistyped command word would run install and
the word would be dropped silently.

<details markdown>
<summary>Help, version and argument edge cases</summary>

| Case | Decided by | Result |
| --- | --- | --- |
| `--help` parsed before a bad root flag | urfave | root help, exit 0 |
| `-h <word>` | urfave's help lookup | that command's help, exit 0; `No help topic`, exit 2 |
| `--version` after a command word | urfave: an undefined flag | exit 2 |
| `-v` after a command word | `UseShortOptionHandling` finds the root flag | ignored, the command runs |
| `explain` with no name or two | `explainArguments` | exit 2 |

</details>

## Requirements file discovery

`config.RequirementsPath` implements
[Which file is read](../guides/requirements.md#which-file-is-read) and never fails.

| Caller | Commands | Its warning |
| --- | --- | --- |
| `newConfigFromCLI`, first step of `BuildCollectionConfig` | install, warm, lock, outdated, cleanup | queued first on `Config.Warnings`, printed by `WarnConfig` |
| the command's own action | hash, tree, explain | `progress.Warnf`, before any output |

- The picked path stays relative and is only a name: a missing file fails
  where the command reads it, exit 2.
- `requirements.Load` picks the parser by `projectfile.IsTOMLPath`, the
  extension alone.
- A `.toml` path also feeds `projectfile.LoadSettings`: in
  `BuildCollectionConfig`, and in `lockfilePath` unless `--lock-file` is set.
  One that does not load exits 2.
- cleanup mounts the flag only so a `galaxy.toml` can name its cache; roots
  come from each recorded project.

## Shared setup

```mermaid
flowchart TD
  C1["BuildCollectionConfig, see<br/>Configuration resolution"] -->|"refused"| XC(["exit 2"])
  C1 --> C2["printer, Galaxy client,<br/>infra.New, git and url clients"]
  C2 --> C3["DebugConfigSources,<br/>WarnConfig"]
  C3 --> C4{"which command?"}
  C4 -->|"outdated"| XOD(["collections.Outdated:<br/>no backend, no lock"])
  C4 -->|"warm --no-cache"| XW(["exit 2,<br/>ErrWarmCacheDisabled"])
  C4 -->|"cleanup"| C5["initCleanup"]
  C4 -->|"install, warm, lock"| C6["withBackend: banner,<br/>then initInstall"]
  C6 --> C7["dry-run banner; warn when<br/>--refresh meets --offline"]
  C7 --> C8["cache.New, Open, Lock, see<br/>Backend open and lock"]
  C5 --> C8
  C8 -->|"refused"| XB(["exit 1, 2, 4<br/>or 8 by cause"])
  C8 --> C9{"which command?"}
  C9 -->|"cleanup"| C10["LoadStore,<br/>LoadProjectRegistry"]
  C9 -->|"install, warm, lock"| C11["sweep dead-run temps,<br/>LoadStore"]
  C10 -->|"failed"| XP(["exit 2, 4, 8<br/>or 9 by cause"])
  C11 -->|"failed"| XP
  C11 --> C12["--clear-cache unless --dry-run:<br/>ClearCaches, ClearFiles"]
  C12 -->|"ClearFiles failed"| XP
  C12 --> C13["RecordProject unless<br/>--dry-run, failure warns"]
  C13 --> XI(["command work under<br/>the holder context"])
  C10 --> XCL(["cleanup work"])
```

`runCollectionCommand` builds everything before the command's own work.
`--offline` swaps the Galaxy client for `fetch.NewOffline` and makes the url
client refuse every request; the extracted store is nil under `--no-cache`.

### Configuration resolution

`BuildCollectionConfig` refuses in this order, each refusal exit 2, so a
configuration broken in several places always reports the same one first.
Construction and precedence are on [Configuration loading](config-loading.md)
and [Where a setting comes from](../reference/configuration.md#where-a-setting-comes-from).

| Order | Step | Refuses |
| ---: | --- | --- |
| 1 | `loadProjectSettings` | a `galaxy.toml` that does not decode, breaks the schema or names an unset `${VAR}` |
| 2 | `applyTimeout` | `--timeout` not a positive integer or Go duration |
| 3 | `loadAnsibleConfigFromCLI` | a missing `--ansible-config`; any ansible.cfg that cannot be read to the end |
| 4 | `resolveServers` | a malformed server, `ErrAmbiguousGalaxyToken`, `ErrInsecureTokenTransport`, a refused token pairing |
| 5 | `loadGitCredentials`, `loadURLCredentials` | a malformed `GO_GALAXY_GIT_*` or `GO_GALAXY_URL_*` binding |
| 6 | `loadS3CacheConfig` | a bucket without both keys, `ErrS3EmptyCreds` |
| 7 | `applySignatureConfig` | a refused keyring, count, status code or `ANSIBLE_GALAXY_DISABLE_GPG_VERIFY` |
| 8 | `applyAnsibleTimeout` | a bad `[galaxy] server_timeout`, read when no `--timeout` source is set |
| 9 | `checkS3CacheOffline` | an S3 bucket beside `--offline`, `ErrS3CacheOffline` |

### Backend open and lock

```mermaid
flowchart TD
  B1{"S3 bucket set?"} -->|"yes"| B2["s3.New over the<br/>Galaxy HTTP client"]
  B1 -->|"no"| B3["local.New at<br/>the cache directory"]
  B2 --> B4["wrap: WithStateDeadline,<br/>WithCleanSaveSkip outermost"]
  B3 --> B4
  B4 --> B5["Open"]
  B5 -->|"failed"| XO(["exit 1, 2 or 4"])
  B5 --> B6["Lock"]
  B6 -->|"refused"| XL(["exit 8 when held,<br/>2 or 4 on failure"])
  B6 -->|"granted"| B7["setup, then the work,<br/>under the holder context"]
  B7 --> B8{"LockLostError: lock lost,<br/>run not canceled?"}
  B8 -->|"yes"| X8(["exit 8,<br/>ErrCacheLockLost"])
  B8 -->|"no"| XR(["the result's<br/>own class"])
```

Every failure after `Open` unwinds through `unwindBackend`: release, then
Close. On the work path the deferred cleanup removes unclaimed `--no-cache`
builds, closes, then releases, a release failure only printed. Only the S3
lock can be lost mid-run ([Cache and storage](cache.md)).

### Exits

| Exit | Decided in | Cause |
| --- | --- | --- |
| 1 | S3 `newClient`, in `Open` | an endpoint `url.Parse` rejects |
| 2 | `BuildCollectionConfig`, `runWarm` | a refused setting; `warm --no-cache` |
| 2 | `Open`, `Lock`, `LoadStore`, `ClearFiles` | `ErrCacheBackendUnusable`; a newer snapshot schema |
| 4 | the same, and every state operation | `ErrCacheBackendUnavailable`, `ErrStateObjectDeadline` |
| 8 | `Lock`, the Bolt open | another holder, the S3 wait ceiling |
| 9 | `LoadStore`, `LoadProjectRegistry` | a corrupt or oversized snapshot, registry or state object |

The local backend's `classifyCacheFailure` maps a permission error to unusable
and anything else to unavailable, so both backends exit alike.

## Which command mounts which flags

| Flags | install, warm | lock | outdated | cleanup | hash, tree, explain |
| --- | :---: | :---: | :---: | :---: | :---: |
| Root: `--verbose`, `-q`, `--dry-run`, `--cache-dir` | yes | yes | yes | yes | accepted, unread |
| `-r` (`--requirements-file`, `--role-file`) | yes | yes | yes | yes | yes |
| `--lock-file` | yes | yes | yes | - | yes |
| Paths, servers, pools, cache behavior, `--offline`, `--metrics-file` | yes | yes | yes | - | - |
| `--frozen` | yes | - | yes | - | - |
| `--check` | - | yes | - | - | - |
| Signature flags | yes | - | - | - | - |
| `--s3-*` | yes | yes | yes | yes | - |

A flag a command does not mount is undefined after its command word (exit 2).
`BuildCollectionConfig` reads it as its zero value and ignores its variable,
so a command must mount every flag whose `Config` field it reads.

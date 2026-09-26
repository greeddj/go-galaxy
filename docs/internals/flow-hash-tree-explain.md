# hash, tree and explain flows

These three commands read files only: no `BuildCollectionConfig`, no
`ansible.cfg`, no cache backend, no network, no write. Options:
[`hash`, `tree` and `explain`](../reference/cli.md#hash-tree-and-explain); what they
print: [Inspect what is locked](../guides/lockfile.md#inspect-what-is-locked) and
[A cache key for CI](../guides/lockfile.md#a-cache-key-for-ci).

## The lockfile path

```mermaid
flowchart TD
    R["config.RequirementsPath:<br/>-r, else discovery"] --> L{"--lock-file set?<br/>an empty value counts"}
    L -->|"yes"| P1["ResolveDefaultPath with it;<br/>empty: galaxy.lock beside req"]
    L -->|"no"| T{"req ends in .toml?"}
    T -->|"no"| P2["galaxy.lock beside req,<br/>or cwd if req is empty"]
    T -->|"yes"| S["projectfile.LoadSettings:<br/>schema, expand the table"]
    S -->|"fails"| X2(["exit 2"])
    S -->|"lock_file set"| P3["lock_file, a relative one<br/>joined under req's directory"]
    S -->|"absent file or unset"| P2
```

All three call `lockfilePath` with the path `config.RequirementsPath` picked
and print its both-files warning. The whole `[tool.go-galaxy]` table is held
to its schema and expanded, so an unset `${VAR}` anywhere in it fails even
`hash`, although only `lock_file` is read.

Flag, help and argument checks run before the action, as drawn on
[Command flows](commands.md): a bad flag or a stray word exits 2, `--help`
exits 0. The global options change nothing; only an unparseable
`GO_GALAXY_VERBOSE`, `GO_GALAXY_QUIET` or `GO_GALAXY_DRY_RUN` exits 2.

## hash

```mermaid
flowchart TD
    LP["lockfilePath"] -->|"fails"| X2(["exit 2"])
    LP --> LD{"lockfile.Load"}
    LD -->|"exists, unreadable<br/>or invalid"| X6(["exit 6"])
    LD -->|"loaded"| H["File.Hash: canonical copy,<br/>two-space YAML, SHA256"]
    LD -->|"does not exist"| RQ["requirements.Read:<br/>raw bytes, never parsed"]
    RQ --> SH["SHA256 of the bytes"]
    RQ -->|"missing or<br/>unreadable"| X2R(["exit 2"])
    H --> PR["print sha256:hex"]
    SH --> PR
    PR --> X0(["exit 0"])
```

`File.Hash` canonicalizes before encoding, so the key ignores entry order and
the written `schema_version` ([Lockfile format](lockfile-format.md)). A
`requirements.yml` that is not YAML still hashes, while a `galaxy.toml` that
is not TOML exits 2 in `lockfilePath` unless `--lock-file` is set.

The action ignores its context, so a caught signal can still print the key
while the exit reports the interrupt.

| Exit | Decided in | Cause |
| --- | --- | --- |
| 2 | `lockfilePath` | `galaxy.toml` settings refused |
| 2 | `requirements.Read` | fallback file missing, or `ErrRequirementsUnreadable` |
| 6 | `lockfile.Load` | lockfile exists but is unreadable or invalid |
| 1 | `File.Hash` | encoder error, unreachable in practice |

| Flag | Diagram | Effect |
| --- | --- | --- |
| `-r` | The lockfile path, hash | the file hashed without a lockfile; the default lockfile's directory |
| `--lock-file` | The lockfile path, hash | a path that does not exist falls back to hashing `-r` |

## tree

```mermaid
flowchart TD
    LP["lockfilePath"] -->|"fails"| X2(["exit 2"])
    LP --> LR["lockfile.LoadRequired"]
    LR -->|"missing or invalid"| X6(["exit 6"])
    LR --> RQ["loadRootFQDNs: requirements.Load,<br/>TOML by extension, else YAML"]
    RQ -->|"missing, unreadable or refused"| X2
    RQ --> PT["printTree: header, one tree<br/>per sorted collection root"]
    PT --> RG{"lockfile or requirements<br/>has roles?"}
    RG -->|"yes"| PR["printRoleTree under roles:"]
    RG -->|"no"| X0(["exit 0"])
    PR --> X0
```

The lockfile loads first, so with both files missing the exit is 6. Under
`--lock-file` a `galaxy.toml` is still decoded for roots, schema-checked but
never expanded. `loadRootFQDNs` picks the roots, shared with `explain`:

| Requirement | Root |
| --- | --- |
| Galaxy | `namespace.name` |
| git (`gitRootFQDNs`) | locked git entries from its URL at its subdir or a child, filtered by name; else name or locator |
| url (`urlRootFQDNs`) | the first locked url entry from its URL; else its `url+` locator |
| role | its install name |

`walkTree` keeps a seen set per root: a shared dependency prints in full under
each root, `(*)` marks a repeat within one, and a root or dependency the
lockfile lacks prints `(missing in lockfile)`. `gitOrigin` and `roleOrigin`
append provenance; output goes through `safeout.NewWriter`, since a lockfile
may be hand-edited.

| Exit | Decided in | Cause |
| --- | --- | --- |
| 2 | `lockfilePath`, `loadRootFQDNs` | `galaxy.toml` settings, or a requirements file missing, unreadable or refused |
| 6 | `lockfile.LoadRequired` | `ErrLockfileMissing`, or invalid |

| Flag | Diagram | Effect |
| --- | --- | --- |
| `-r` | The lockfile path, tree | the file the roots come from |
| `--lock-file` | The lockfile path, tree | the lockfile drawn |

## explain

```mermaid
flowchart TD
    A{"explainArguments:<br/>exactly one name?"} -->|"none, or two or more"| X2(["exit 2"])
    A -->|"one"| LP["lockfilePath"]
    LP -->|"fails"| X2
    LP --> LR["lockfile.LoadRequired"]
    LR -->|"missing or invalid"| X6(["exit 6"])
    LR --> RQ["loadRootFQDNs,<br/>its error discarded"]
    RQ --> FD["findExplainTarget,<br/>findExplainRole"]
    FD -->|"neither found"| X1(["exit 1"])
    FD --> PS["print the collection section,<br/>then the role section"]
    PS --> X0(["exit 0"])
```

- `explainArguments` replaces the root's `NoArguments`, runs before any file
  is read, and names every extra word rather than answer only the first.
- A broken requirements file leaves both root sets empty, so a real root
  prints as an orphan instead of failing, unlike `tree`.
- A role matches by install or Galaxy name, the last match winning; parents
  match its install name, which is what role deps hold.
- The collection header prints `type`, `source`, `download_url`, `ref`,
  `commit`, `subdir` and `sha256` when set; the role header always prints
  `type` and `source`.
- The `(root)` line names `filepath.Base` of the requirements path.

| Exit | Decided in | Cause |
| --- | --- | --- |
| 2 | `explainArguments` | `ErrMissingArgument`, or `ErrUnexpectedArguments` |
| 2 | `lockfilePath` | `galaxy.toml` settings refused |
| 6 | `lockfile.LoadRequired` | lockfile missing or invalid |
| 1 | `printExplain` | `errExplainNotFound`, which wraps no sentinel |

| Flag | Diagram | Effect |
| --- | --- | --- |
| `-r` | The lockfile path, explain | the roots and the `(root)` label |
| `--lock-file` | The lockfile path, explain | the lockfile searched |

# CLI

Every go-galaxy command and option, with its variables, file keys and default.
Start with the task table, then look up the command or option group.

## Usage

| Task | Command |
| --- | --- |
| Install the requirements file | `go-galaxy install` |
| Pin every version | `go-galaxy lock` |
| Install in CI from the lockfile | `go-galaxy install --frozen` |
| Fail CI when the lockfile is stale | `go-galaxy lock --check` |
| Fill a CI image's cache | `go-galaxy warm` |
| See newer upstream versions | `go-galaxy outdated` |
| Show the locked dependency tree | `go-galaxy tree` |
| Why a collection is there | `go-galaxy explain community.general` |
| Print a CI cache key | `go-galaxy hash` |
| Free disk space | `go-galaxy cleanup` |

> [!TIP]
> A bare `go-galaxy` runs `install`, so `go-galaxy -r galaxy.toml` is
> `go-galaxy install -r galaxy.toml`.

## Commands

| Command | Alias | Does | Network | Cache lock | Writes |
| --- | --- | --- | --- | --- | --- |
| `install` | `i` | Installs collections and roles | unless `--offline` | yes | collections and roles paths, cache |
| `lock` | `l` | Resolves and writes the lockfile | unless `--offline` | yes | `galaxy.lock`, cache |
| `warm` | `w` | Fills the cache without installing | unless `--offline` | yes | cache |
| `outdated` | `o` | Compares what runs with the latest upstream | always | no | `--metrics-file` only |
| `cleanup` | `c` | Removes what no recorded project reaches | S3 only | yes | deletes installs and cache entries |
| `hash` | `h` | Prints a CI cache key | no | no | nothing |
| `tree` | `t` | Prints the locked dependency tree | no | no | nothing |
| `explain` | `why` | Shows why one entry is locked | no | no | nothing |

No command but `explain <name>` takes an argument. Every name comes from the
requirements file, and an extra word exits [`2`](exit-codes.md).

<details markdown>
<summary>How arguments are dispatched</summary>

- The first word that is neither a [global option](#global-options) nor a
  command hands that word and everything after it to `install`.
- `go-galaxy -r galaxy.toml lock` therefore hands `lock` to `install`, which
  refuses it (`2`), as it refuses `go-galaxy collection install ns.name`.
- `go-galaxy help` exits `2` the same way; help is `--help`.
- An unknown flag before any command word is reported against `install`'s
  flag set; after one, against that command's.
- `explain` with no name or with two exits `2`.
- `--version` and `-v` work only before a command word; after one,
  `--version` exits `2` and `-v` is ignored.
- `go-galaxy -h <command>` prints that command's help; a word naming no
  command exits `2` with `No help topic`.

</details>

### `install`

```bash
go-galaxy install --frozen
```

`install` takes every [option group](#options) except `lock`'s `--check`.
Roles install after the collections, and only when every collection
succeeded. `--frozen` installs from the lockfile: [Install from the
lockfile](../guides/lockfile.md#install-from-the-lockfile).

### `lock`

```bash
go-galaxy lock --check
```

`lock` resolves from the requirements file, not the existing lockfile, and pins
every entry. Unchanged requirements replay the [last
resolution](../guides/caching.md#what-a-rerun-reuses) unless `--refresh` or
`--no-cache` is set. `lock` takes the `install` options except `--frozen` and
the signature flags.

`--check` compares instead of writing and exits `6` on drift. `--dry-run`
prints the difference. Workflow: [Create the
lockfile](../guides/lockfile.md#create-the-lockfile) and [Catch
drift](../guides/lockfile.md#catch-drift).

### `warm`

```bash
go-galaxy warm --frozen
```

`warm` downloads and extracts every collection and role into the cache
without installing, so a later `install` skips both steps.
It takes the `install` options and needs a cache, so `--no-cache` exits `2`.

`cleanup` keeps what a recent `warm` extracted: [Freshness and
retention](../guides/caching.md#freshness-and-retention). Recipe: [Container
image bake](../guides/ci.md#container-image-bake).

### `outdated`

```bash
go-galaxy outdated
```

`outdated` compares the lockfile's entries, or without one the collections
under `--download-path`, with the latest upstream: [Find newer
versions](../guides/lockfile.md#find-newer-versions).

`--offline` exits `4`. `outdated` warns once that `--clear-cache`,
`--no-cache`, `--refresh`, `--no-deps`, `--frozen` and `--s3-bucket` do
nothing.

### `cleanup`

```bash
go-galaxy cleanup --dry-run
```

```text
· Would remove community.general@13.4.0
· Would remove community.library_inventory_filtering_v1@1.1.5
· Would remove role geerlingguy.docker
· Would sweep extracted "cbb9e86c5b1720df21e940cedcd2f3e1226c38262623e090a883712610733851"
✔ Dry-run cleanup complete. Candidates: 3
```

The count leaves out `Would sweep` lines.

`cleanup` removes what no recorded project reaches, from every project and
from the cache. What stays, and when `cleanup` warns or stops: [What cleanup
keeps](../guides/caching.md#what-cleanup-keeps).

`cleanup` takes the global options, `-r` and the [S3](#s3) flags. `-r` names
the cache to clean, through a `galaxy.toml`'s `cache_dir` and `s3`. It does
not narrow `cleanup` to that project.

`cleanup` reads `[galaxy] cache_dir` from an `ansible.cfg` it finds by
discovery alone. It takes no `--ansible-config` and ignores
`GO_GALAXY_ANSIBLE_CONFIG`. When `install` named its file that way, point
`cleanup` at it with `ANSIBLE_CONFIG`, or pass `--cache-dir` ([Where it is
found](configuration.md#where-it-is-found)).

`cleanup` uses no server or credential, but it checks these as `install` does,
and a broken one exits `2`:

- the servers `galaxy.toml` and `ansible.cfg` name
- `ANSIBLE_GALAXY_SERVER`, `ANSIBLE_GALAXY_SERVER_LIST` and
  `ANSIBLE_GALAXY_SERVER_<ID>_*`
- the git and url credentials in `GO_GALAXY_GIT_*` and `GO_GALAXY_URL_*`

### `hash`, `tree` and `explain`

```bash
go-galaxy explain community.general
```

These read files only. They take only `-r` and `--lock-file`, and ignore the
global options. `explain` still answers when the entries of the requirements
file do not load. Its `required by` list then does not name that file.

What each prints and when it fails: [Inspect what is
locked](../guides/lockfile.md#inspect-what-is-locked) and [A cache key for
CI](../guides/lockfile.md#a-cache-key-for-ci).

## Global options

| Flag | Variables | Effect |
| --- | --- | --- |
| `--help`, `-h` | | Prints the command's help; `go-galaxy -h <command>` works too |
| `--version`, `-v` | | Prints the version; before a command word only |
| `--verbose` | `GO_GALAXY_VERBOSE` | Adds debug lines: where each setting came from, requests, timings |
| `--quiet`, `-q` | `GO_GALAXY_QUIET` | Drops progress lines; results, warnings and errors still print. Ignored with `--verbose` |
| `--dry-run` | `GO_GALAXY_DRY_RUN` | Reports what would happen; see [Dry run](#dry-run) |
| `--cache-dir` | `GO_GALAXY_CACHE_DIR`, `ANSIBLE_GALAXY_CACHE_DIR` | The [local cache](../guides/caching.md#the-local-cache); unset, `galaxy.toml`'s `cache_dir`, then `ansible.cfg`'s `[galaxy] cache_dir`, then `~/.cache/go-galaxy` |

## Options

A flag outranks its variables, listed here in precedence order, and the files
come after: [Where a setting comes
from](configuration.md#where-a-setting-comes-from). A `[section] key` is
`ansible.cfg`'s; a bare key sits in `galaxy.toml`'s `[tool.go-galaxy]` table.

| Group | `install` | `warm` | `lock` | `outdated` | `cleanup` | `hash`, `tree`, `explain` |
| --- | --- | --- | --- | --- | --- | --- |
| [Global](#global-options) | yes | yes | yes | yes, `--cache-dir` ignored | yes | ignored |
| [Paths and files](#paths-and-files) | yes | yes | yes | yes | `-r` only | `-r` only |
| [Servers and network](#servers-and-network) | yes | yes | yes | yes | | |
| [Concurrency](#concurrency) | yes | yes | yes | `--download-workers` ignored | | |
| [Cache behavior](#cache-behavior) | yes | `--no-cache` exits `2` | yes | ignored; `--offline` exits `4` | | |
| [Lockfile](#lockfile) | `--lock-file`, `--frozen` | `--lock-file`, `--frozen` | `--lock-file`, `--check` | `--lock-file`; `--frozen` ignored | | `--lock-file` |
| [Signatures](#signatures) | yes | yes | | | | |
| [S3](#s3) | yes | yes | yes | validated, then ignored | yes | |

Blank: the command does not take the flag, so passing it exits `2`, and its
variables are not read. Ignored: accepted with no effect. `outdated` warns
once about some of its ignored flags, listed under [`outdated`](#outdated).
Every other ignored flag is silent.

### Paths and files

| Flag | Variables | File key | Default | Meaning |
| --- | --- | --- | --- | --- |
| `--download-path`, `-p` | `GO_GALAXY_COLLECTIONS_PATH`, `GO_GALAXY_DOWNLOAD_PATH`, `ANSIBLE_COLLECTIONS_PATH` | `[defaults] collections_path` | `.collections` | Where collections install |
| `--roles-path` | `GO_GALAXY_ROLES_PATH`, `ANSIBLE_ROLES_PATH` | `[defaults] roles_path` | `.roles` | Where roles install |
| `--requirements-file`, `-r`, `--role-file` | `GO_GALAXY_REQUIREMENTS_FILE`, `ANSIBLE_GALAXY_REQUIREMENTS_FILE` | | `galaxy.toml`, else `requirements.yml` | The requirements file; a `.toml` name reads as `galaxy.toml` |
| `--ansible-config` | `GO_GALAXY_ANSIBLE_CONFIG` | | discovered | An `ansible.cfg` that must exist, else exit `2`; a missing [`ANSIBLE_CONFIG`](configuration.md#where-it-is-found) is skipped |
| `--metrics-file` | `GO_GALAXY_METRICS_FILE` | `metrics_file` | | Writes a JSON [run report](metrics.md) |

Either install path is read as ansible's `:` list: the first entry wins and
the rest draw a warning. A roles path equal to the collections path warns too,
and roles install beside `ansible_collections`. Roles path warnings print only
when the run has roles.

Every command loads a `galaxy.toml`'s
[`[tool.go-galaxy]`](configuration.md#the-toolgo-galaxy-table) table before it
checks any other setting, and a `galaxy.toml` that fails to load exits `2`.
The exception is `hash`, `tree` and `explain` under `--lock-file`: they do not
load the table, so an unset `${VAR}` does not stop them. Discovery: [Which file is
read](../guides/requirements.md#which-file-is-read).

### Servers and network

| Flag | Variables | File key | Default | Meaning |
| --- | --- | --- | --- | --- |
| `--server` | `GO_GALAXY_SERVER` | `servers`, `[galaxy] server_list`, `[galaxy] server` | `https://galaxy.ansible.com` | A server URL, or the id of a configured server |
| `--token` | `GO_GALAXY_TOKEN` | | | API token for the one server in effect |
| `--timeout` | `GO_GALAXY_SERVER_TIMEOUT`, `GO_GALAXY_TIMEOUT`, `ANSIBLE_GALAXY_SERVER_TIMEOUT` | `[galaxy] server_timeout` | `30s` | Longest wait for headers, or between two body reads |

`--timeout` takes seconds (`60`) or a Go duration (`1m30s`). `0`, a negative
or an unparseable value exits `2` (`invalid timeout`). It bounds a stall, not
a transfer: [Timeouts and fixed limits](#timeouts-and-fixed-limits).

`ANSIBLE_GALAXY_SERVER_LIST`, even empty, outranks the server list from
`servers` and from `[galaxy] server_list`. `ANSIBLE_GALAXY_SERVER` outranks
`[galaxy] server`. `--token` with several servers in effect exits `2`, and an
empty value clears a configured token. The rules: [Which servers a run
uses](../guides/servers-and-auth.md#which-servers-a-run-uses) and [Where a
token may go](../guides/servers-and-auth.md#where-a-token-may-go).

### Concurrency

| Flag | Variable | File key | Default | Accepted | Outside that |
| --- | --- | --- | --- | --- | --- |
| `--workers` | `GO_GALAXY_WORKERS` | `workers` | CPUs, clamped to 2..16 | 1 to max(CPUs, 2) | One warning, then the default |
| `--download-workers` | `GO_GALAXY_DOWNLOAD_WORKERS` | `download_workers` | 4 x CPUs, clamped to 8..32 | 1 or more | The default, silently |

CPUs means those this process may use, a container's quota included. A
non-integer value exits `2`.

`--workers` bounds installs and extraction, which wait on the filesystem. It
also bounds dry-run probes, the metadata requests sent before resolving and
every `outdated` lookup. `--download-workers` bounds downloads, cache probes,
version-list pages and the fetches that resolve git, url and role sources.
Against a server that throttles, lower both. Where creating files is slow,
fewer workers can be faster: [Why your numbers will
differ](benchmarks.md#why-your-numbers-will-differ). Internals: [Two pools, two
resources](../internals/install-pipeline.md#two-pools-two-resources).

### Cache behavior

| Flag | Variable | Effect |
| --- | --- | --- |
| `--no-cache` | `GO_GALAXY_NO_CACHE` | Bypasses the artifact cache and extracted store, and resolves afresh |
| `--refresh` | `GO_GALAXY_REFRESH` | Re-asks version-free answers: version lists, branches, tags, the v1 role API, url sources |
| `--clear-cache` | `GO_GALAXY_CLEAR_CACHE` | Before the run, deletes cached metadata, the commits and sha256s the cache recorded, and artifacts, S3 included. `galaxy.lock` is untouched |
| `--offline` | `GO_GALAXY_OFFLINE` | Uses cached state only; any network access fails |
| `--no-deps` | `GO_GALAXY_NO_DEPS` | Installs only the requirements file's own entries |

`--offline` beats `--refresh` with a warning. `--refresh` and `--no-deps` do
nothing under `--frozen`: pass them to `lock` instead ([What a frozen install
checks](../guides/lockfile.md#what-a-frozen-install-checks)). Compared: [Cache
flags](../guides/caching.md#cache-flags).

### Lockfile

| Flag | Variable | File key | Meaning |
| --- | --- | --- | --- |
| `--lock-file` | `GO_GALAXY_LOCK_FILE` | `lock_file` | The lockfile to read or write |
| `--frozen` | `GO_GALAXY_FROZEN` | | `install`, `warm`: take every entry from the lockfile, pins enforced |
| `--check` | `GO_GALAXY_CHECK` | | `lock`: compare with the lockfile instead of writing; drift exits `6` |

The path is the first of:

1. `--lock-file` or `GO_GALAXY_LOCK_FILE` when set. An empty value skips to
   step 3.
2. `lock_file` in `galaxy.toml`, relative to that file.
3. `galaxy.lock` beside the requirements file.

`lock --frozen` exits `2`, and `GO_GALAXY_FROZEN` never reaches `lock`. What
the pins enforce: [What a frozen install
checks](../guides/lockfile.md#what-a-frozen-install-checks).

### Signatures

| Flag | Variables | Default | Meaning |
| --- | --- | --- | --- |
| `--keyring` | `GO_GALAXY_KEYRING`, `ANSIBLE_GALAXY_GPG_KEYRING` | unset: no checks | OpenPGP keyring to verify against |
| `--required-valid-signature-count` | `GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT`, `ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT` | `1` | A count or `all`; a `+` prefix also demands one valid signature |
| `--ignore-signature-status-code` | `GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE`, `ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES` | | A status code to tolerate, such as `NO_PUBKEY`; repeatable |
| `--disable-gpg-verify` | `GO_GALAXY_DISABLE_GPG_VERIFY`, `ANSIBLE_GALAXY_DISABLE_GPG_VERIFY` | `false` | Skips verification even with a keyring |

No file sets these: an `ansible.cfg`'s signature keys draw a warning. How
verification works: [Turning it on](../guides/signatures.md#turning-it-on).

### S3

| Flag | Variables | `[tool.go-galaxy.s3]` key | Default | Meaning |
| --- | --- | --- | --- | --- |
| `--s3-bucket` | `GO_GALAXY_S3_BUCKET` | `bucket` | | Turns the S3 cache on |
| `--s3-region` | `GO_GALAXY_S3_REGION` | `region` | `us-east-1` | Signing region |
| `--s3-prefix` | `GO_GALAXY_S3_PREFIX` | `prefix` | | Key prefix inside the bucket |
| `--s3-endpoint` | `GO_GALAXY_S3_ENDPOINT` | `endpoint` | AWS, for the region | An S3-compatible endpoint |
| `--s3-access-key` | `GO_GALAXY_S3_ACCESS_KEY`, `AWS_ACCESS_KEY_ID` | `access_key` | | Required with a bucket |
| `--s3-secret-key` | `GO_GALAXY_S3_SECRET_KEY`, `AWS_SECRET_ACCESS_KEY` | `secret_key` | | Required with a bucket |
| `--s3-session-token` | `GO_GALAXY_S3_SESSION_TOKEN`, `AWS_SESSION_TOKEN` | `session_token` | | For temporary credentials |
| `--s3-path-style-disabled` | `GO_GALAXY_S3_PATH_STYLE_DISABLED` | `path_style_disabled` | path style | Addresses `<bucket>.<endpoint>` instead |

A bucket without both keys, or with `--offline`, exits `2`. Pass secrets as
variables: other processes can see flags. Setup: [S3 cache
(optional)](../guides/caching.md#s3-cache-optional).

### Timeouts and fixed limits

| Budget | Covers | Value | Configurable |
| --- | --- | --- | --- |
| `--timeout` | The wait for headers once a request is sent, and each gap between body reads | `30s` | yes |
| Connection | Opening a TCP connection to any origin, ssh git included | 10 s | no |
| TLS handshake | Every https connection: Galaxy, S3, git, url, signatures | 3 s | no |
| Artifact acquisition | One artifact's download, S3 cache included, extraction, commit and retries | 15 min | no |
| Metadata request | One Galaxy API request, retries included. All pages of one collection's version list share one budget | 2 min | no |
| Cache-state operation | One snapshot or registry load or save | 1 min | no |
| Signature phase | Every signature source of one collection | 1 min | no |

A budget that runs out fails the item it bounds. In `install` and `warm`, the
run then ends with the summary error `installation failed` and exits `5`
([When several things fail](exit-codes.md#when-several-things-fail)). A budget
that ends the run on its own, such as during resolution, exits `4`. Neither
counts as an interrupt. The messages: [Messages to
grep](exit-codes.md#messages-to-grep).

<details markdown>
<summary>Why these values</summary>

`--timeout` catches a stalled server, not one that drips a few bytes into
every window, so each fixed ceiling bounds the whole operation. None is
configurable: a knob on a safety ceiling is the one an operator raises after a
truncation, which is how a slow-drip attack wins.

15 minutes moves 4 GiB, the size cap on one artifact, at about 38 Mbit/s,
while a real collection needs under 1 Mbit/s. An ordinary collection uses
that budget once. A failed prefetch, or a cached copy that fails its check,
gets a second budget. Internals: [Acquiring an
artifact](../internals/install-pipeline.md#acquiring-an-artifact).

The state budget is tighter because it runs under the cache lock, which
blocks every other run sharing the cache.

</details>

## Dry run

```text
$ go-galaxy lock --dry-run
! --dry-run is active: no Galaxy artifact will be downloaded and no artifact installed or cached; a role, git source or url source with no usable recorded pin is still fetched to learn its identity, then discarded; the resolved metadata caches are still saved
```

`install`, `warm` and `lock` print this banner on stderr, even under
`--quiet`. The `lock` report that follows: [Catch
drift](../guides/lockfile.md#catch-drift).

> [!WARNING]
> An exported `GO_GALAXY_DRY_RUN` turns every `install`, `warm`, `lock` and
> `cleanup` in a job into a dry run. Nothing is installed, warmed, locked or
> removed, yet a job that would have succeeded still exits `0`. `lock --check`
> still fails on drift. The sign is the banner above, or `cleanup`'s
> `Dry-run cleanup complete`.

| Command | Prints | Still does | Never does |
| --- | --- | --- | --- |
| `install` | `Would install`, `Up to date` or `Would fail`; roles add `(role)` | Takes the cache lock; fetches git, url and role sources with no recorded commit or sha256 | Downloads Galaxy artifacts; writes installs, extracted trees or records |
| `warm` | `Would warm`, `Already warm` or `Would fail`; roles add `(role)` | As `install` | Writes warmed entries or artifacts |
| `lock` | `Would add`, `Would update`, `Would remove` and a verdict | Resolves like a real run | Writes the lockfile |
| `cleanup` | `Would remove`, `Would sweep` and a candidate count | Takes the cache lock | Deletes or saves anything |
| `outdated` | Its usual report | Every lookup | Writes `--metrics-file` |

`install`, `warm` and `lock` also skip `--clear-cache` and the metrics report,
and save the metadata caches only when a snapshot already exists. A would-fail
exits with the code a real run hitting the same cause uses:

| Would fail because | Exit |
| --- | --- |
| Not cached, under `--offline` | `5` |
| Cached digest contradicts the lockfile pin, under `--offline` | `7` |
| `install`: a file or escaping symlink blocks the install path | `5` |
| `install`: a role directory neither go-galaxy nor `ansible-galaxy` installed | `5` |
| `install`: `ansible_collections` is a bad symlink, or a file | `5`, or `1` for a file |

<details markdown>
<summary>Known blind spots</summary>

A dry run checks that a cached artifact exists and compares its recorded
digest with the lockfile pin. It does not re-hash the bytes. Under
`--frozen --offline`, a tarball whose bytes drifted still reports as cached,
and one fixed warning says the real run could still fail on such bytes.

`warm` reports `Already warm` only when the extracted tree is ready under a
sha256 it knows without a download: the locked one, a url source's, or the
one the last `warm` recorded. Extracted trees stay local, so a fresh runner
over a warm bucket reports `Would warm: <name> (artifact cached)`. Internals:
[Dry run](../internals/install-pipeline.md#dry-run).

</details>

## Output and color

```text
· Resolve dependencies
· Downloading https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/artifacts/community-general-13.4.0.tar.gz
✔ Installed: community.general == 13.4.0
✔ Installed: role geerlingguy.docker == 8.0.0
✔ All done. Took 26s
```

| Marker | Means | Stream |
| --- | --- | --- |
| `✔` | A result | stdout |
| `↑` | A newer version exists (`outdated`) | stdout |
| `✗` | A failure | stderr |
| `!` | A warning | stderr |
| `·` | Progress, or a report line such as `Would remove` or a run's summary. `--quiet` drops only progress | stdout |
| `== <version>` | The version a result settled on | its line's |

Each stream is colored only when it is a terminal: with
`go-galaxy install > install.log` the log is plain and stderr keeps its color.
The spinner draws only on a terminal stdout, never under `--quiet` or
`--verbose`.

| Variable | Effect |
| --- | --- |
| `NO_COLOR` | Non-empty: never color; beats the two below. The spinner still runs, uncolored |
| `CLICOLOR_FORCE`, `FORCE_COLOR` | Non-empty and not `0`: color even off a terminal |
| `TERM=dumb` | Removes the spinner's color only; markers keep theirs |

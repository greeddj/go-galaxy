# Upgrading

What to change when a project, a runner or a shared cache moves to a newer
go-galaxy release. One rule holds at every upgrade. Every job and machine that
shares a cache or a lockfile runs the same release, since an older binary
refuses what a newer one writes ([Pin one release](../guides/ci.md#pin-one-release)).

## From v1.4.x

Start here from v1.4.0. From an older release, follow
[From v1.3.x](#from-v13x) first.

The tables below list every change since v1.4.0 that can break a job or change
its result, except an input that used to be refused and is now accepted.

### Exit codes that changed

Update any CI step that branches on the old code for these cases
([Using exit codes in CI](exit-codes.md#using-exit-codes-in-ci)).

| Situation | Was | Now |
| --- | --- | --- |
| A git collection or role whose `version:` is a commit the remote does not serve, on a remote that allows fetching by hash, outside `--frozen` | `4`, a git transport failure naming `not our ref` | `3`, as on a remote that does not fetch by hash |
| A `galaxy.lock` git collection, git role or Galaxy role entry whose `ref` is a commit other than its `commit`, or two url collection entries with one `source` | `0` in most cases: `install --frozen` installed the entry's `commit`, not the one its `ref` names, and with two url entries a `version:` matching only one of them passed or failed at random | `6`, as for any `galaxy.lock` that does not load |

What to do about a new code:

- `3` for `does not hold commit`: point `version:` at a commit the repository
  holds, or at a branch or tag.
- `6` for `is a commit and differs from commit` or
  `url entry locked from the same source as`: run `go-galaxy lock` to rewrite
  `galaxy.lock`, then review and commit it.

## From v1.3.x

Start here from v1.3.0 or v1.3.1, and follow [From v1.4.x](#from-v14x) next.
From an older release, follow [From v1.2.x](#from-v12x) first.

The tables below list every change since v1.3.1 that can break a job or change
its result, except an input that used to be refused and is now accepted.

| Change | What you do | If you do not |
| --- | --- | --- |
| An unquoted value in `requirements.yml` reads as the text written, as a role's `meta/main.yml` already did: `version: 1.10` asks for `1.10`, not `1.1`, and `version: 1.0` for `1.0.x`, not every `1.x`. A number, `true` or a date under a collection's `namespace:`, `source:` or `type:`, ignored before, is read and judged ([Version constraints](../guides/requirements.md#version-constraints)) | Before the new release runs `install`, `lock` or `cleanup` over a project, quote each unquoted numeric value as the text you mean: `"1.1"` and `"1"` keep what `1.10` and `1.0` asked for. Then relock: run `go-galaxy lock` and commit `galaxy.lock` ([Create the lockfile](../guides/lockfile.md#create-the-lockfile)) | `--frozen` runs and `lock --check` exit `6` where `galaxy.lock` no longer matches, such as a role locked at `1.1`. A plain `install` takes another version, a lower one for `1.0`, and `cleanup` removes an installed version the new reading no longer reaches |
| A key an entry is read by, written with no value, such as `version:`, `name: ~` or `signatures: null`, and a list or a mapping under a key that takes text, such as `type: [git]`, exit `2` at load. Before, a collection's `version:` with no value read as the text `<nil>`, and most other such keys, a list under `type:` included, read as absent ([What is refused](../guides/requirements.md#what-is-refused)) | Write the value, or delete the key. `version: ""` still means no version | `install`, `warm`, `lock`, `hash` with no `galaxy.lock` and, beside one, `tree` exit `2` before anything installs. `cleanup` exits `2` and deletes nothing while a recorded project's file holds such a collection entry. One in `roles:` keeps that project's roles, with a warning |
| `go-galaxy hash` with no `galaxy.lock` keys on the collections and roles the requirements file asks for, not on its bytes, so its key changes once and then stays across comments, formatting, collection order, a respelled constraint and a move to `galaxy.toml` ([A cache key for CI](../guides/lockfile.md#a-cache-key-for-ci)) | Expect one CI cache miss where the key does not carry the go-galaxy release. The action's key carries it | Nothing: the miss costs one cold run |
| `cleanup` reads the `galaxy.toml`, `requirements.yml` and `galaxy.lock` in a project's directory in place of a remembered requirements file that is gone, and stops when nothing is left to read ([What cleanup keeps](../guides/caching.md#what-cleanup-keeps)). Before, a gone file kept nothing, so deleting `requirements.yml` before a run recorded the `galaxy.toml` beside it removed that project's collections. A project whose directory is gone keeps nothing, as before, with one warning instead of one per file | To retire a project whose directory stays, delete its entry from `<cache_dir>/projects.json` or from the `state/projects.json` object on S3 | Its installs stay, and `cleanup` exits `2` while nothing is left to read there |

### Exit codes that changed

Update any CI step that branches on the old code for these cases
([Using exit codes in CI](exit-codes.md#using-exit-codes-in-ci)).

| Situation | Was | Now |
| --- | --- | --- |
| A number, `true` or a date under a collection's `namespace:` or `type:`, such as `type: 1` | `0`, the key ignored | `2`, at load |
| A Galaxy collection entry whose `version:` has no value, or holds a list or a mapping | `1`, at the resolve | `2`, at load |
| A git collection entry whose `version:` has no value | `3`, no ref named `<nil>` | `2`, at load |
| A role entry key, or a collection's `namespace:`, `source:`, `type:` or `signatures:`, written with no value, or a list or a mapping under a collection's `namespace:`, `source:` or `type:` | `0`, read as absent | `2`, at load |
| `hash` with no `galaxy.lock`, over a requirements file that does not load, such as one that is not YAML or names a `type: file` source | `0`, a key over the file's bytes | `2` |
| A lockfile that is a named pipe, such as `--lock-file <(...)` or a `galaxy.lock` made by `mkfifo` | `0` once a writer feeds the pipe. Without one, the run blocks, and `install --frozen` and `warm --frozen` hold the cache lock meanwhile, so other runs on that cache exit `8` | `6`, before the pipe is opened, from `hash`, `tree`, `explain`, `outdated`, `lock --check`, `install --frozen` and `warm --frozen` |
| `cleanup` where a project's directory remains but no requirements file it remembers is left, and it holds no `galaxy.toml`, `requirements.yml` or `galaxy.lock` | `0`, removing what only those files reached | `2`, removing nothing, under `--dry-run` too |
| `cleanup` where a remembered requirements file is gone and a `galaxy.toml` or `requirements.yml` beside it does not load | `0` | `2` |
| `cleanup` where a remembered requirements file is gone and the `galaxy.lock` beside it does not load or is not a regular file | `0` | `6` |
| A Galaxy collection's `source:` that is neither an id of a server the run uses nor an http(s) URL, such as an id no server list names, an id beside a `--server` URL, a host name with no scheme or an `ftp://` URL | `4` from `install`, `warm` and `lock`, after trying the value as a URL. `install --frozen` and `warm --frozen` ignored it and fetched from the URL `galaxy.lock` records, with no token: `0` where that server needs none. A number, such as an unquoted `source: 123`, was ignored and the collection taken from the default server: `0` | `2`, before any request, `--frozen` included |

What to do about a new code:

- `2` for `has no value` or `not a string` on a requirements entry: write the
  value the named key needs, or delete the key.
- `2` from `hash` with no `galaxy.lock`: fix the requirements file as `install`
  would ask, since `install` refuses it too. To key another tool's cache on a
  file go-galaxy does not install, hash the file itself, such as with
  `hashFiles()`.
- `2` from `cleanup` for
  `recorded project has no requirements file or lockfile`: restore the file,
  or write the `galaxy.toml` that replaced it, in the directory the message
  names. For a directory no run uses any more, delete the project's entry from
  the registry the message names, `<cache_dir>/projects.json` or the
  `state/projects.json` object on S3.
- `2` for `unknown collection source`: give the run a server with that id, or
  write the server's URL in `source:`. Beside a `--server` URL no id matches:
  pass the id itself to `--server`, or drop the `source:`
  ([How a collection picks its server](../guides/servers-and-auth.md#how-a-collection-picks-its-server)).
- `6` for `lockfile is invalid: <path> is not a regular file`: write what the
  pipe carries to a regular file and name that one with `--lock-file` or
  `lock_file`.
- `6` from `cleanup`: fix the `galaxy.lock` the message names, or move it away.

## From v1.2.x

Start here from v1.1.0 through v1.2.3, and read [From v1.3.x](#from-v13x)
before step 3. Upgrade in four steps:

1. Pin the release you run now in every place
   [Pin one release](../guides/ci.md#pin-one-release) lists, so no job takes
   the new one early. A `greeddj/go-galaxy@v1` step with no `version:` input
   takes each release as it ships.
2. In one change, move every pin to the new release and upgrade every other
   go-galaxy binary, developer machines included. An older `lock` writes
   Galaxy entries without `download_url`, and the new release refuses such a
   file.
3. In the same change, run `go-galaxy lock` with the new release, review the
   diff and commit `galaxy.lock`. `lock` never reads the old file, so on a
   cold cache it resolves against the servers and can move versions. On
   v1.1.0 to v1.2.2, the default lockfile was `requirements.lock.yml`. No
   later release looks for it, so delete it.
4. In the same change, replace `lock --frozen` with
   [`lock --check`](../guides/lockfile.md#catch-drift) and
   `$GO_GALAXY_FROZEN` with `$GO_GALAXY_CHECK` on every `lock` step.
   `install`, `warm` and `outdated` keep `--frozen`.

The `lock` of step 3 exits `5` when a server names a `download_url` that
carries a query string or leaves the server's origin. Such a server
works only without a lockfile
([Create the lockfile](../guides/lockfile.md#create-the-lockfile)).

The tables below list every change since v1.2.3 that can break a job or change
its result, except an input that used to be refused and is now accepted.

| Change | What you do | If you do not |
| --- | --- | --- |
| A `galaxy.lock` holding a Galaxy collection records its `download_url`, as `schema_version: 5` | Relock, as step 3 says | `install --frozen`, `warm --frozen`, `lock --check`, `hash`, `tree`, `explain` and `outdated` exit [`6`](exit-codes.md#special-cases-by-command) on the older file |
| `lock --frozen` is now `lock --check`, read from `$GO_GALAXY_CHECK` | Change every `lock` step, as step 4 says | `lock --frozen` exits `2`. Under `$GO_GALAXY_FROZEN`, `lock` rewrites the lockfile and passes |
| A `lock` [metrics report](metrics.md#fields) never carries `frozen` | Tell a `--check` run from a write by its job, not its report | A dashboard counting `lock` reports with `frozen` counts none |
| `lock` writes a two-space indent, so `go-galaxy hash` and `lockfile_hash` change once | Expect one CI cache miss. A lockfile with no Galaxy entry gets a whitespace-only diff from its next `lock` | Nothing: the indent alone refuses no file |
| `galaxy.toml` is read ahead of `requirements.yml` | Upgrade every binary sharing the cache before a project moves, then follow [Moving to galaxy.toml](../guides/requirements.md#moving-to-galaxytoml) | An older release reads only `requirements.yml` and exits `2` without it. Its `cleanup` exits `2` on that cache and deletes nothing |
| An extra word on the command line exits `2`. Before, it was ignored, so `go-galaxy help` and `install --no-deps true` ran `install`. So did `go-galaxy -r requirements.yml lock`, since a command's own option written before the command word sends the whole line to `install` | Drop the extra words, and write a command's options after its name ([Commands](cli.md#commands)) | The step exits `2` before doing anything |
| An `ansible.cfg` header with spaces inside its brackets, such as `[ galaxy ]`, names another section, as in ansible ([What go-galaxy reads](configuration.md#what-go-galaxy-reads)) | Write `[galaxy]` and `[galaxy_server.<id>]` with no inner spaces | Keys under `[ galaxy ]` are ignored with no warning, so a `server_list` there is lost and the run falls back to galaxy.ansible.com. A `server_list` id whose section is spaced exits `2` |
| A relative `collections_path`, `roles_path` or `[galaxy] cache_dir` in `ansible.cfg` resolves from the file's directory, not the working directory. `~` and `$VAR` expand in these keys, in their `ANSIBLE_*` variables and in `ANSIBLE_CONFIG`, and an `ANSIBLE_CONFIG` naming a directory reads the `ansible.cfg` in it, as in ansible ([ansible.cfg paths](configuration.md#ansiblecfg-paths)) | Where a job runs outside its `ansible.cfg`'s directory, or a path holds `~` or `$VAR`: delete the trees the old paths made, such as `.collections` and `.roles` in the working directory, or the directory `./~` the old release created (`rm -rf -- ./~`, never a bare `~`), and move CI cache and artifact paths to the new places | The first run installs again into the new place, and a cache moved this way starts cold. A step still reading the old place, such as a CI cache or a playbook pointed there, finds stale content or none |
| `install --no-cache` and `lock --no-cache` resolve again instead of replaying the [last resolution](../guides/caching.md#what-a-rerun-reuses), so they can take newer versions | Where versions must not move, drop `--no-cache`, or install from `galaxy.lock` with `--frozen` | A `--no-cache` run installs or locks newer versions than the last run did |
| `--no-deps` with several servers binds an exact pin to the first server that has the collection. Before, it took the first server without asking | Nothing. The first `--no-deps` run after the upgrade resolves again instead of replaying the last resolution | - |
| The action fails at its cache-key step when `go-galaxy hash` fails. Before, it keyed the cache on an empty hash and went on, so two kinds of job newly fail: `frozen: false` over a lockfile that does not load, which at this release is every v1.2.x lockfile holding a Galaxy collection, and a requirements file named only through `args` where neither the [discovered](../guides/requirements.md#which-file-is-read) file nor a `galaxy.lock` beside it exists | Relock, as step 3 says, name the file with the `requirements` input, or set `cache: false` ([Inputs and outputs](../guides/ci.md#inputs-and-outputs)) | The action fails before anything installs |
| `cleanup` scans only the collections and roles paths a project's latest `install` resolved from its working directory ([What cleanup keeps](../guides/caching.md#what-cleanup-keeps)). Before, a relative path was resolved from the requirements file's directory, so a project installed from outside it, as with `-r sub/requirements.yml`, was never found, and `lock` and `warm` rewrote the paths too. Where that collections path was missing or held no `ansible_collections`, or none was recorded, `cleanup` scanned `.collections`, then `collections`, beside the requirements file, so a vendored `collections/` there lost every collection no project reached | Run `install` once with the new release in each project whose installs `cleanup` should prune, which rewrites its record | `cleanup` scans only the path the old record names, if any: it removes none of that project's unused installs anywhere else, and on a shared cache it can delete cached artifacts that project still uses |
| `cleanup` removes a collection copy only while go-galaxy's extract marker matches its tree, and keeps what a kept copy depends on ([What cleanup keeps](../guides/caching.md#what-cleanup-keeps)). Before, it removed every unreached copy under a recorded collections path, another tool's included | Delete by hand any copy you want gone that another tool installed or changed, or that v1.0.x installed and no later `install` redid | Those copies stay on disk, and `cleanup` never removes them |

### Exit codes that changed

Update any CI step that branches on the old code for these cases, and retry
`4` as you retry other network failures
([Using exit codes in CI](exit-codes.md#using-exit-codes-in-ci)).

| Situation | Was | Now |
| --- | --- | --- |
| A Galaxy server that cannot be reached (connection, DNS, TLS) while resolving or locking | `1` | `4` |
| Any status but `404` on a versions page, a version document or a role's v1 versions page | `1` | `4` |
| At an [API root](../guides/servers-and-auth.md#api-roots-and-url-normalization) or a v1 role lookup, any status but `401`, `403`, `404`, `429`, `500`, `502`, `503` and `504` | `1` | `4` |
| A metadata document that is not JSON, or JSON of the wrong shape, such as a timestamp that does not parse | `1` | `4` |
| Web pages at every API root of a server | `1` | `4` |
| A collection no server has, where a server serves web pages at an API root, as galaxy.ansible.com does under `/v3` | `1` | `3` |
| An exact pin no server has, or a versions page or version document that answers `404` | `1` | `3` |
| A role whose versions answer `404` on the first server that lists it | `2`, `3`, or `0` with the role taken from a later server | `3` |
| Under `--offline`, a url role whose `version:` was changed, added or removed since the cache recorded the role. Without `--offline`, the role is now downloaded again and installs | `2` | `4` |
| Under `--offline` (`install` or `warm`, with or without `--frozen`), a git collection whose commit the cache recorded, or `galaxy.lock` pins under `--frozen`, but whose artifact is not cached. A commit the cache never recorded still exits `4` without `--frozen` | `0`, the artifact fetched from its repository | `5`, with no request to the repository |
| `lock`: a collection or version the resolve chose is gone from its server | `1` | `3` |
| `--no-deps` with several servers: an exact pin no server has, or a server failing while the pin is looked up | `5` from `install` and `warm`, `0` from a dry run | `3`, or `4` for the failing server |
| `warm` over a dependency cycle, which `install` already refused | `0`, or `5` under `--offline` with a collection uncached | `3` |
| A requirements file or `ansible.cfg` that exists but cannot be read, or a requirements file that is not YAML | `1` | `2` |
| `explain` with no name | `1` | `2` |
| `--offline` beside an S3 bucket | `4` | `2`, before any request |
| An S3 listing or batch-delete reply that does not decode or breaks off, or a `412` to a PUT that sets no precondition | `1` | `4` |
| An S3 snapshot object that inflates but does not decode | `1` | `9` |
| A Galaxy `sha256` in `galaxy.lock` that is set but not 64 lowercase hex digits | `7`, at install | `6`, when the file loads |
| `-r` naming a file with no `.yml`, `.yaml` or `.toml` ending, such as `requirements` | `0` from every command but `cleanup`, which had no `-r`. Without a `galaxy.lock` beside the file, `6` from `tree` and `explain`, and from `outdated` with no collections installed | `2` |
| `-r /dev/stdin` or `-r <(...)` | `0` from `install`, `warm` and `hash`, and from `outdated` over installed collections, else `6`; `1` or `2` from `lock`; `6` from `tree` and `explain` | `2` |
| An exported-empty `GO_GALAXY_REQUIREMENTS_FILE` or `ANSIBLE_GALAXY_REQUIREMENTS_FILE` | `0` from `cleanup`. Beside a `galaxy.lock` in the working directory, `0` from `outdated`, `hash` and `explain`. Without one, `6` from `tree` and `explain`, and from `outdated` `0` over installed collections, else `6` | `2` |
| `-r` naming a named pipe, such as one `mkfifo requirements.yml` made | `0` from `install`, `warm` and `lock`, from `hash` with no `galaxy.lock` beside the pipe and from `tree` beside one, once a writer feeds the pipe. Without a writer, the run blocks | `2` |
| `cleanup` after an `install` from one requirements file, where an earlier run in the same directory loaded another that still exists but no longer loads, or an older release's run failed on one, such as `-r galaxy.yml` | `0` | `2` |

What to do about a new code:

- `2` for `requirements file name must end in .yml, .yaml or .toml`: rename
  the file, write a stream to such a file first, or unset an exported-empty
  variable.
- `2` for `requirements file is not a regular file`: write what the pipe
  carries to a regular file and name that one.
- `2` from `cleanup` for `project requirements file is unreadable`: fix the
  file the message names, or move it away if no run reads it any more. For a
  file the project needs that holds no requirements, such as the `galaxy.yml`
  an older `install -r galaxy.yml` recorded, delete the project's entry from
  `<cache_dir>/projects.json`, or from the `state/projects.json` object on S3,
  once ([What cleanup keeps](../guides/caching.md#what-cleanup-keeps)).
- `3` for a collection: fix its name, or pin a version its server publishes.
- `3` from `lock` for a version gone from its server: the run has usually
  replayed the last resolution. Run `go-galaxy lock --refresh` to ask the
  servers again.
- `3` for a role whose versions answer `404`: fix its server, or list the
  server that serves the role first
  ([Roles and the v1 role API](../guides/servers-and-auth.md#roles-and-the-v1-role-api)).
- `4` for a url role under `--offline`: run once without `--offline`, so the
  cache records its new `version:`.
- `5` for a git collection under `--offline`: fill the cache without
  `--offline` first. Before a `--frozen` run, use
  [`go-galaxy warm --frozen`](cli.md#warm): a plain `warm` does not read
  `galaxy.lock`, so it can cache another commit than the one the file pins.
  Before any other run, use `go-galaxy warm`.
- `6` for a malformed `sha256`: `lock` never writes one, so the file was edited
  by hand. Run `go-galaxy lock` to rewrite it.
- `9` for an S3 snapshot: delete the S3 key the message names.

## From v1.0.x

The first `install`, `lock` or `warm` rebuilds the cache's metadata once
(snapshot schema 9), and `cleanup` does not sweep extracted trees until the
first `install` or `warm`. Check the changes below and the rest of the
[v1.1.0 release notes](https://github.com/greeddj/go-galaxy/releases/tag/v1.1.0),
then read [From v1.2.x](#from-v12x).

<details markdown>
<summary>What trips a v1.0.x pipeline</summary>

| Change | What you do |
| --- | --- |
| `$ANSIBLE_GALAXY_SERVER` acts as `[galaxy] server`, below `server_list`, not as `--server` | Set `$GO_GALAXY_SERVER` instead |
| [`--workers`](cli.md#concurrency) below 1 or above the permitted CPU count (at least 2) gets the default and a warning | Stay in that range |
| Downloads follow `--download-workers` (default 8 to 32), not `--workers` | Lower it, and `--workers`, if your server throttles ([Concurrency](cli.md#concurrency)) |
| `install --dry-run` and `$GO_GALAXY_DRY_RUN` preview and install nothing | Keep both out of install jobs: they still exit `0` |
| A `roles:` list is installed, and one bad entry fails the file | Fix entries go-galaxy [refuses](../guides/requirements.md#what-is-refused) |
| An environment token beside an `ansible.cfg` server `url` or `validate_certs = false` exits `2` | Export that address or TLS setting yourself ([Where a token may go](../guides/servers-and-auth.md#where-a-token-may-go)) |

</details>

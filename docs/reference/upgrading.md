# Upgrading

What to change when a project, a runner or a shared cache moves to a newer
go-galaxy release. One rule holds at every upgrade. Every job and machine that
shares a cache or a lockfile runs the same release, since an older binary
refuses what a newer one writes ([Pin one release](../guides/ci.md#pin-one-release)).

## From v1.2.x

Start here from any release since v1.1.0. Upgrade in four steps:

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
carries a query string or is not the server's own artifact URL. Such a server
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
| `install --no-cache` and `lock --no-cache` resolve again instead of replaying the [last resolution](../guides/caching.md#what-a-rerun-reuses), so they can take newer versions | Where versions must not move, drop `--no-cache`, or install from `galaxy.lock` with `--frozen` | A `--no-cache` run installs or locks newer versions than the last run did |
| `--no-deps` with several servers binds an exact pin to the first server that has the collection. Before, it took the first server without asking | Nothing. The first `--no-deps` run after the upgrade resolves again instead of replaying the last resolution | - |
| The action fails at its cache-key step when `go-galaxy hash` fails. Before, it keyed the cache on an empty hash and went on, so two kinds of job newly fail: `frozen: false` over a lockfile that does not load, which at this release is every v1.2.x lockfile holding a Galaxy collection, and a requirements file named only through `args` where neither the [discovered](../guides/requirements.md#which-file-is-read) file nor a `galaxy.lock` beside it exists | Relock, as step 3 says, name the file with the `requirements` input, or set `cache: false` ([Inputs and outputs](../guides/ci.md#inputs-and-outputs)) | The action fails before anything installs |

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

What to do about a new code:

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

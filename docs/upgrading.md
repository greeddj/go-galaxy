# Upgrading

What to change when a project, a runner or a shared cache moves to a newer
go-galaxy release. One rule holds at every upgrade: every job and machine that
shares a cache or a lockfile runs the same release, since an older binary
refuses what a newer one writes ([Pin one release](ci.md#pin-one-release)).

## From v1.2.x

| Change | What you do | If you do not |
| --- | --- | --- |
| A `galaxy.lock` holding a Galaxy collection records its `download_url`, as `schema_version: 5` | In one change, upgrade every binary and commit the file [`go-galaxy lock`](lockfile.md#create-the-lockfile) rewrites | `install --frozen`, `warm --frozen`, `lock --check`, `hash`, `tree`, `explain` and `outdated` exit [`6`](exit-codes.md) on the older file |
| `lock --frozen` is now `lock --check`, read from `$GO_GALAXY_CHECK` | Spell [`--check`](lockfile.md#catch-drift) in every `lock` step, or set `$GO_GALAXY_CHECK`; `install`, `warm` and `outdated` keep `--frozen` | `lock --frozen` exits `2`; under `$GO_GALAXY_FROZEN`, `lock` rewrites the lockfile and passes |
| A `lock` [metrics report](metrics.md#fields) never carries `frozen` | Tell a `--check` run from a write by its job, not its report | A dashboard counting `lock` reports with `frozen` counts none |
| `lock` writes a two-space indent, so `go-galaxy hash` and `lockfile_hash` change once | Expect one CI cache miss, and a whitespace-only diff from the next `lock` where nothing else changed | Nothing: the indent alone refuses no file |
| `galaxy.toml` is read ahead of `requirements.yml` | Upgrade every binary sharing the cache before a project moves, then follow [Moving to galaxy.toml](requirements.md#moving-to-galaxytoml) | An older release reads only `requirements.yml`, exiting `2` without it; its `cleanup` exits `2` on that cache, deleting nothing |
| An unreachable Galaxy server (connection, DNS, TLS), or an API root answering a status other than `404`, `401` or `403`, exits [`4`](exit-codes.md), not `1` | Retry exit `4` as you retry other network failures | A step that branched on `1` for an outage no longer matches |
| A collection no server has exits `3`, not `1`, where a server also serves its web UI at an API root, as galaxy.ansible.com does under `/v3`; a server answering web pages at every API root exits `4` | Nothing, unless a step matched exit `1` for a misspelled name | That step no longer matches |

Every change above is new since v1.2.3. On v1.1.0 to v1.2.2, also read the
breaking changes in the
[v1.2.3 release notes](https://github.com/greeddj/go-galaxy/releases/tag/v1.2.3).

> [!WARNING]
> An older `lock`, on a developer machine too, rewrites `galaxy.lock` without
> `download_url`, and every upgraded job then exits `6` on it. A
> `greeddj/go-galaxy@v1` step with no `version:` input takes each release as
> it ships: [pin it](ci.md#pin-one-release) and bump it with the relock.

Review the relock's diff: `lock` never reads the old file, so on a cold cache
it resolves against the servers and can move versions. A server whose
`download_url` carries a query string or is not its own artifact URL cannot be
locked (exit `5`, [details](lockfile.md#create-the-lockfile)).

## From v1.0.x

The first `install`, `lock` or `warm` rebuilds the cache's metadata once
(snapshot schema 9), and `cleanup` skips its extracted-cache sweep until the
first `install` or `warm`. Check the changes below and the rest of the
[v1.1.0 release notes](https://github.com/greeddj/go-galaxy/releases/tag/v1.1.0),
then read [From v1.2.x](#from-v12x).

<details markdown>
<summary>What trips a v1.0.x pipeline</summary>

| Change | What you do |
| --- | --- |
| `$ANSIBLE_GALAXY_SERVER` acts as `[galaxy] server`, below `server_list`, not as `--server` | Set `$GO_GALAXY_SERVER` instead |
| [`--workers`](cli.md#concurrency) below 1 or above the permitted CPU count (at least 2) gets the default and a warning | Stay in that range |
| Downloads follow `--download-workers` (default 8 to 32), not `--workers` | Lower it if your server throttles |
| `install --dry-run` and `$GO_GALAXY_DRY_RUN` preview and install nothing | Keep both out of install jobs: they still exit `0` |
| A `roles:` list is installed; one bad entry fails the file | Fix entries go-galaxy [refuses](requirements.md#what-is-refused) |
| An environment token beside an `ansible.cfg` server `url` or `validate_certs = false` exits `2` | Export that address or TLS setting yourself ([fixes](servers-and-auth.md#--token)) |

</details>

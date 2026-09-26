# Upgrading

What to change when a project, a runner or a shared cache moves to a newer
go-galaxy release. One rule holds at every upgrade: every job and machine that
shares a cache or a lockfile runs the same release, since an older binary
refuses what a newer one writes ([Pin one release](../guides/ci.md#pin-one-release)).

## From v1.2.x

| Change | What you do | If you do not |
| --- | --- | --- |
| A `galaxy.lock` holding a Galaxy collection records its `download_url`, as `schema_version: 5` | In one change, upgrade every binary and commit the file [`go-galaxy lock`](../guides/lockfile.md#create-the-lockfile) rewrites | `install --frozen`, `warm --frozen`, `lock --check`, `hash`, `tree`, `explain` and `outdated` exit [`6`](exit-codes.md) on the older file |
| `lock --frozen` is now `lock --check`, read from `$GO_GALAXY_CHECK` | Spell [`--check`](../guides/lockfile.md#catch-drift) in every `lock` step, or set `$GO_GALAXY_CHECK`; `install`, `warm` and `outdated` keep `--frozen` | `lock --frozen` exits `2`; under `$GO_GALAXY_FROZEN`, `lock` rewrites the lockfile and passes |
| A `lock` [metrics report](metrics.md#fields) never carries `frozen` | Tell a `--check` run from a write by its job, not its report | A dashboard counting `lock` reports with `frozen` counts none |
| `lock` writes a two-space indent, so `go-galaxy hash` and `lockfile_hash` change once | Expect one CI cache miss, and a whitespace-only diff from the next `lock` where nothing else changed | Nothing: the indent alone refuses no file |
| `galaxy.toml` is read ahead of `requirements.yml` | Upgrade every binary sharing the cache before a project moves, then follow [Moving to galaxy.toml](../guides/requirements.md#moving-to-galaxytoml) | An older release reads only `requirements.yml`, exiting `2` without it; its `cleanup` exits `2` on that cache, deleting nothing |
| A Galaxy metadata request failing in a resolve or in `lock` exits [`4`](exit-codes.md), not `1`: an unreachable server (connection, DNS, TLS); any status but `404` on a versions page, a version document or a role's v1 versions page; a document that is not JSON or has the wrong shape, a timestamp that does not parse included; and at an API root or v1 role lookup any status but `404`, where `401`, `403`, `429`, `500`, `502`, `503` and `504` already exited `4` | Retry exit `4` as you retry other network failures | A step that branched on `1` for an outage no longer matches |
| A `404` the resolve meets exits [`3`](exit-codes.md), not `1`: an exact pin no server has, a versions page or version document gone. So does a role whose versions the server listing it answers `404` for: that server was passed over as having no v1, so the run exited `2` or `3`, or took the role from a later server | Pin a version the server publishes, or fix the name; for that role, fix the server or list the one serving it first in [`server_list`](../guides/servers-and-auth.md#how-a-collection-picks-its-server) | A step that branched on `1`, or on `2` for that role, no longer matches; a run that took the role from a later server exits `3` |
| `--no-deps` with several servers binds an exact pin to the first server that has the collection, as a resolve with dependencies does, where it took the first server unasked; a failed server walk now ends the resolve before anything installs: exit [`4`](exit-codes.md) for a failing server, `3` for a pin no server has, where `install` and `warm` exited `5` for that item and a dry run `0` | Nothing: the first `--no-deps` run after the upgrade resolves again rather than replaying the cache | A step that branched on `5` or `0` for those no longer matches |
| A collection no server has exits `3`, not `1`, where a server also serves its web UI at an API root, as galaxy.ansible.com does under `/v3`; a server answering web pages at every API root exits `4` | Nothing, unless a step matched exit `1` for a misspelled name | That step no longer matches |
| An S3 snapshot object that inflates but does not decode exits [`9`](exit-codes.md), not `1` | Delete the S3 key the message names, as for any exit `9` | A step that branched on `1` for it no longer matches |
| `lock` exits [`3`](exit-codes.md), not `1`, when a collection or version the resolve named is gone from its server, as after a resolve replayed from the cache | Relax the constraint or pick a published version, then `lock` again | A step that branched on `1` for it no longer matches |
| A Galaxy entry in `galaxy.lock` whose `sha256` is set but not 64 lowercase hex digits is refused on load, exit [`6`](exit-codes.md), where an install failed on it with `7` | Nothing for a file `lock` wrote; `go-galaxy lock` rewrites a hand-edited one | `install --frozen`, `warm --frozen`, `lock --check`, `hash`, `tree`, `explain` and `outdated` exit `6` on that file |

Every change above is new since v1.2.3. On v1.1.0 to v1.2.2, also read the
breaking changes in the
[v1.2.3 release notes](https://github.com/greeddj/go-galaxy/releases/tag/v1.2.3).

> [!WARNING]
> An older `lock`, on a developer machine too, rewrites `galaxy.lock` without
> `download_url`, and every upgraded job then exits `6` on it. A
> `greeddj/go-galaxy@v1` step with no `version:` input takes each release as
> it ships: [pin it](../guides/ci.md#pin-one-release) and bump it with the relock.

Review the relock's diff: `lock` never reads the old file, so on a cold cache
it resolves against the servers and can move versions. A server whose
`download_url` carries a query string or is not its own artifact URL cannot be
locked (exit `5`, [details](../guides/lockfile.md#create-the-lockfile)).

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
| A `roles:` list is installed; one bad entry fails the file | Fix entries go-galaxy [refuses](../guides/requirements.md#what-is-refused) |
| An environment token beside an `ansible.cfg` server `url` or `validate_certs = false` exits `2` | Export that address or TLS setting yourself ([fixes](../guides/servers-and-auth.md#--token)) |

</details>

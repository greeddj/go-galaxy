<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-white.svg">
    <img src="docs/assets/logo.svg" alt="" width="72" align="absmiddle">
  </picture>&nbsp;go-galaxy
</h1>

[![CI](https://github.com/greeddj/go-galaxy/actions/workflows/ci.yml/badge.svg)](https://github.com/greeddj/go-galaxy/actions/workflows/ci.yml)
[![Release](https://github.com/greeddj/go-galaxy/actions/workflows/release.yml/badge.svg)](https://github.com/greeddj/go-galaxy/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/greeddj/go-galaxy/graph/badge.svg)](https://codecov.io/gh/greeddj/go-galaxy)

Fast Ansible Galaxy collections and roles installer for CI.

> [!NOTE]
> This project was created in collaboration with Claude Code.

CI pipelines often spend minutes downloading and unpacking Galaxy collections
and roles. go-galaxy resolves, downloads and extracts them in parallel,
hardlinks out of a content-addressed cache on warm runs, and skips the network
entirely under `--frozen --offline`. With a lockfile and warm caches,
installing 100 collections takes seconds rather than minutes.

![go-galaxy against ansible-galaxy, install speedup by cache state](docs/benchmark.svg)

*Mean of 5 runs on Linux, both tools with `--no-deps`; the warm rows compare
[two cache designs](docs/benchmarks.md#what-each-tool-caches).*

## Why go-galaxy

- **Shared cache.** An optional [S3 bucket](docs/caching.md#s3-cache-optional)
  shares downloads across runners, and [`cleanup`](docs/cli.md#cleanup-options)
  removes what no project uses any more.
- **Reproducible.** `galaxy.lock`
  [pins](docs/lockfile.md#what-each-entry-is-pinned-by) every collection and
  role to an exact version, commit or sha256.
- **Refuses rather than guesses.** A PubGrub resolver backtracks to older
  releases until the constraints fit, or prints a
  [proof](docs/requirements.md#when-no-version-fits) that none do and exits
  [`3`](docs/exit-codes.md).
- **Reads your ansible-galaxy setup.** `requirements.yml`, and the
  `ansible.cfg` keys and `ANSIBLE_*` variables for paths, servers and
  timeouts, plus its own [`galaxy.toml`](docs/requirements.md#galaxytoml).
- **Many sources.** Collections and roles from Galaxy, git and http(s)
  tarballs; with a [keyring](docs/signatures.md), Galaxy collection signatures
  are checked in pure Go, without `gpg`.

Some differences break a pipeline that worked under `ansible-galaxy`: read
[Differences a migration runs into](docs/ansible-galaxy-compat.md#differences-a-migration-runs-into)
before you switch.

## Install

| Method | Command |
| :-- | :-- |
| GitHub Action | `uses: greeddj/go-galaxy@v1` runs a cached `install` on Linux or macOS; `install: false` only puts it on PATH ([inputs](docs/ci.md#inputs-and-outputs)) |
| Homebrew | `brew install --cask greeddj/tap/go-galaxy` |
| Go | `go install github.com/greeddj/go-galaxy/cmd/go-galaxy@latest` |
| Release binary | the `curl` lines below |
| Container | `ghcr.io/greeddj/go-galaxy`, run in your project as below |
| From source | `go build -o dist/go-galaxy ./cmd/go-galaxy` in a clone |

```bash
curl -sSLf -o /usr/local/bin/go-galaxy \
  https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
chmod +x /usr/local/bin/go-galaxy
```

Swap `linux-amd64` for `linux-arm64`, `darwin-amd64` or `darwin-arm64`; the
darwin builds need macOS 13 or later.

```bash
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/work \
  -v "$PWD":/work -w /work ghcr.io/greeddj/go-galaxy install
```

The image runs as a non-root user: `-u` lets it write into your project, and
`HOME` puts its cache in `.cache` there.

<details markdown>
<summary>Homebrew quarantine and verifying a release</summary>

The builds are not Apple-notarized, so on macOS the cask clears the quarantine
attribute from the binary it installs, which skips a Gatekeeper check for you.
To check a download's signature and provenance yourself, follow
[Verifying a release](docs/security.md#verifying-a-release).

</details>

## Quick start

```toml
# galaxy.toml
[project]
collections = [
  "community.general >= 10.0.0",
  "ansible.utils",
]
roles = ["geerlingguy.docker"]
```

```bash
go-galaxy install
go-galaxy lock
go-galaxy install --frozen
```

`install` reads `./galaxy.toml`, else `./requirements.yml`, and by default
installs into `.collections` and `.roles`; export
`ANSIBLE_COLLECTIONS_PATH=.collections` and `ANSIBLE_ROLES_PATH=.roles` so
ansible-playbook looks there too. `lock` pins every version in `galaxy.lock`;
commit it, and `install --frozen` installs exactly what it pins.
[Get started](docs/getting-started.md) walks through these steps and a first
CI job.

<details markdown>
<summary>The same project in requirements.yml</summary>

```yaml
collections:
  - name: community.general
    version: ">=10.0.0"
  - name: ansible.utils
roles:
  - name: geerlingguy.docker
```

Write this file instead when `ansible-galaxy` must read it too.

</details>

## Scope

| Source | Collections | Roles | Pinned in `galaxy.lock` by |
| :-- | :-- | :-- | :-- |
| Galaxy API server | yes | yes, through the v1 role API (Automation Hub has none) | collection: version, `download_url`, sha256 if published; role: commit |
| git repository, http(s) or ssh | yes | yes | commit |
| http(s) tarball URL | yes | `.tar.gz` only | sha256 of the downloaded bytes |
| `file`, `dir`, `subdirs`, local path | no | no | - |

Entry syntax for each source, and what is refused at load, is in
[Requirements files](docs/requirements.md); private git and URL credentials
are in [Servers and credentials](docs/servers-and-auth.md).

## Documentation

| Section | Pages |
| :-- | :-- |
| Get started | [Get started](docs/getting-started.md), [Coming from ansible-galaxy](docs/ansible-galaxy-compat.md) |
| Guides | [Requirements files](docs/requirements.md), [Lockfile](docs/lockfile.md), [CI pipelines](docs/ci.md), [Servers and credentials](docs/servers-and-auth.md), [Caching and S3](docs/caching.md), [Signatures](docs/signatures.md), [Security](docs/security.md) |
| Reference | [CLI](docs/cli.md), [Configuration](docs/configuration.md), [Exit codes](docs/exit-codes.md), [Metrics](docs/metrics.md), [Upgrading](docs/upgrading.md), [Benchmarks](docs/benchmarks.md) |
| Internals | [How it works](docs/internals/index.md), [Command flows](docs/internals/commands.md), [Security boundaries](docs/internals/boundaries.md), [Development](docs/internals/development.md) |
| Contributing | [CONTRIBUTING.md](CONTRIBUTING.md): commit subjects and cutting a release |

## License

[MIT](LICENSE).

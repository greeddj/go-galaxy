# go-galaxy

[![CI](https://github.com/greeddj/go-galaxy/actions/workflows/ci.yml/badge.svg)](https://github.com/greeddj/go-galaxy/actions/workflows/ci.yml)
[![Release](https://github.com/greeddj/go-galaxy/actions/workflows/release.yml/badge.svg)](https://github.com/greeddj/go-galaxy/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/greeddj/go-galaxy/graph/badge.svg)](https://codecov.io/gh/greeddj/go-galaxy)

Fast Ansible Galaxy collections and roles installer for CI.

> **Note:** This project was created in collaboration with the Claude Code.

CI pipelines often spend minutes downloading and unpacking Galaxy collections
and roles. go-galaxy resolves, downloads and extracts them in parallel,
hardlinks out of a content-addressed cache on warm runs, and skips the network
entirely under `--frozen --offline`. With a lockfile and warm caches,
installing 100 collections takes seconds rather than minutes - see
[Benchmarks](docs/benchmarks.md).

![go-galaxy against ansible-galaxy, install speedup by cache state and collection count](docs/benchmark.svg)

Five measured runs per point on Linux with xfs, both tools passed `--no-deps`,
against `ansible-galaxy` 2.21.3. Read the warm rows knowing they compare two
designs rather than one design done faster: `ansible-galaxy` caches API
responses only, so a warm run still re-downloads every tarball, while
go-galaxy hardlinks out of an extracted cache and pays one inode per file.
`cmd/go-galaxy-benchmark` reproduces the chart.

It is a drop-in for the `install` subset of `ansible-galaxy`: the same
`requirements.yml` (collections and roles in one file), the same `ansible.cfg`
keys, the same `ANSIBLE_*` environment variables. Where it deliberately
behaves differently - one server per collection rather than a union, a
fail-closed 401/5xx, a resolver that refuses an unsatisfiable constraint set
instead of picking leniently, a role fetched by git at its tag rather than as
a GitHub tarball - every difference is written down in
[Compatibility with ansible-galaxy](docs/ansible-galaxy-compat.md).

## Resolution

Versions are chosen by a PubGrub solver - the algorithm Dart's `pub`
introduced - rather than by walking the requirements and taking the newest
version of each in turn. The difference shows up exactly where the greedy
approach gets stuck.

It backtracks as far as it has to. Asking for the latest `ansible.netcommon`
alongside `ansible.utils` pinned to `1.0.0` does not fail: the solver walks
`ansible.netcommon` back through its release history until it finds one whose
dependencies the pin allows.

It refuses rather than guesses. When no combination satisfies the constraints
there is no lenient fallback and no "closest match" - it prints PubGrub's proof
of why and exits 3:

```
✗ failed to resolve dependencies: Because ansible.netcommon 8.6.2 depends on ansible.utils >=3.0.0 and root depends on ansible.netcommon 8.6.2, ansible.utils >=3.0.0 is forbidden.
So, because root depends on ansible.utils 1.0.0, version solving failed.
```

The solver is pure, in-memory and deterministic, performs no I/O of its own,
and is checked against a brute-force oracle and a fuzzer. See
[How it works](docs/architecture.md#the-version-solver).

## Scope

- Collections from Galaxy API servers, from git repositories (https, ssh,
  public or private), and from direct http(s) tarball URLs (`url` sources: a
  release asset, an artifact store object), pinned in the lockfile by the
  artifact's sha256, with an optional Bearer token bound per origin through
  `GO_GALAXY_URL_*` - see [Servers and auth](docs/servers-and-auth.md).
  `file` and `dir` sources are not supported.
- Roles from the Galaxy v1 role API (`owner.role`, resolved on
  galaxy.ansible.com or a standalone Galaxy NG - Automation Hub has no role
  API), from git repositories (`git+<url>`, `git@host:path`, `scm: git`,
  or a bare `https://github.com/<owner>/<repo>` URL), and from http(s)
  `.tar.gz` URLs, pinned by the origin bytes' sha256. A Galaxy role is
  fetched by git at the tag the v1 API names, never as a GitHub tarball, so
  its lockfile pin is a commit. A role's `meta/main.yml` and
  `meta/requirements.yml` dependencies are installed transitively.
  Local-path role sources, non-http and non-`.tar.gz` role URLs, `scm`
  other than `git`, and `include:` are refused at load.
- Roles install under `roles_path` (`--roles-path`, default `.roles`,
  project-local like `.collections`), one directory per role, with ansible's
  `meta/.galaxy_install_info` written beside the role's meta so
  `ansible-galaxy role list` reads it.
- `requirements.yml` is either a mapping carrying a `collections` list, a
  `roles` list, or both, or a bare top-level list of collection entries;
  anything else is refused.
- `ansible.cfg` is read for `[defaults] collections_path`,
  `[defaults] roles_path`, `[galaxy] server`, `[galaxy] server_list`,
  `[galaxy] cache_dir`, `[galaxy] server_timeout`, and `[galaxy_server.<id>]`
  sections (`url`, `token`, `validate_certs`). Everything else in that file is ignored or refused - see
  [Configuration](docs/configuration.md).
- A token you supply is never paired with a server address, or a relaxed TLS
  policy, that an `ansible.cfg` file chose rather than you. See
  [Galaxy servers and authentication](docs/servers-and-auth.md).

## Features

- PubGrub version solving, complete and deterministic, with snapshot reuse.
- API and tarball caches, local or shared over S3; role artifacts and their
  extracted trees share them.
- Skip install if already extracted.
- Parallel downloads and extraction.
- Lockfile pinning every transitive collection to an exact version + SHA256,
  and every role to a commit.
- OpenPGP signature verification, in pure Go, with no `gpg` process.
- `cleanup` command to remove unreachable collections and roles.

## Install

### GitHub Actions

```yaml
      - uses: greeddj/go-galaxy@v1
        with:
          collections-path: ./collections
          frozen: true
```

A composite action that installs the release binary, checks it against the
release's `checksums.txt`, and keys `actions/cache` on `go-galaxy hash`. Linux
and macOS runners; every input is in
[Reproducible CI](docs/ci.md#github-actions).

### Go

```bash
go install github.com/greeddj/go-galaxy/cmd/go-galaxy@latest
```

Binary is installed into `$(go env GOPATH)/bin` (usually `~/go/bin`).

### Homebrew

```bash
brew install --cask greeddj/tap/go-galaxy
```

A cask rather than a formula, and installed as one explicitly, because of what
it does on macOS: it clears the quarantine attribute from the binary it stages.
These builds are not Apple-notarized and a quarantined one does not run, so
that is a Gatekeeper check being skipped on your behalf. See
[Verifying a release](docs/security.md#verifying-a-release) for what to check
instead. On Linux the cask installs the same archive and the hook does nothing.

### Binary

```bash
curl -sSLf -o /usr/local/bin/go-galaxy \
  https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
chmod +x /usr/local/bin/go-galaxy
```

Substitute `linux-arm64`, `darwin-amd64` or `darwin-arm64` for another
platform; the darwin binaries need macOS 13 or later, the floor of the Go
toolchain they are built with. Each release also carries a `.tar.gz` per
platform with the same binary plus LICENSE and the documentation. See
[Verifying a release](docs/security.md#verifying-a-release) before trusting
either.

### Podman

```bash
podman run --rm -v "$PWD":/work -w /work ghcr.io/greeddj/go-galaxy:latest --help
```

### Build from source

```bash
go build -o ./dist/go-galaxy ./cmd/go-galaxy
```

## Usage

```bash
go-galaxy install -r requirements.yml -p ./collections --roles-path ./roles
```

Running `go-galaxy` with no command runs `install`, so a bare invocation
performs a full install rather than printing help; a word that names no
command is refused as an argument to `install` rather than ignored, since
nothing is named on the command line. One `install` handles the
`collections:` and the `roles:` lists of the same file, as `ansible-galaxy
install -r` does; there is no separate role subcommand.

For reproducible CI, pin every transitive collection and role once and install
from the lockfile thereafter:

```bash
go-galaxy lock                 # writes galaxy.lock
go-galaxy install --frozen     # install exactly the locked versions
```

`go-galaxy` exits with a class-specific code rather than a flat `1`, so a
pipeline can branch on the failure type without parsing log output. See
[Exit codes](docs/exit-codes.md).

## Documentation

| Document | What it covers |
| :-- | :-- |
| [CLI reference](docs/cli.md) | Every command and option, `--dry-run`, output and color |
| [Configuration](docs/configuration.md) | `ansible.cfg` discovery and keys, the environment surface, `requirements.yml` |
| [Galaxy servers and authentication](docs/servers-and-auth.md) | `server_list`, tokens, precedence, TLS, refused configurations |
| [Signature verification](docs/signatures.md) | Keyrings, required counts, tolerated statuses, the manifest chain |
| [Compatibility with ansible-galaxy](docs/ansible-galaxy-compat.md) | Every deliberate difference, and what a migration runs into |
| [Caching](docs/caching.md) | The local cache, the shared S3 backend, `warm` and `cleanup` |
| [Reproducible CI](docs/ci.md) | GitHub Actions, GitLab CI, the lockfile drift gate, image bake |
| [Exit codes](docs/exit-codes.md) | The failure classes and what each one means |
| [Metrics](docs/metrics.md) | The JSON run report |
| [Security](docs/security.md) | Trust model, and verifying a release |
| [Benchmarks](docs/benchmarks.md) | Measurements against `ansible-galaxy`, and how to reproduce them |
| [How it works](docs/architecture.md) | The solver, the install pipeline, the caching model, the layering |
| [Development](docs/development.md) | Running the tests, the repository's own gates, lint, the benchmark harness |
| [Contributing](CONTRIBUTING.md) | Commit subjects the release notes are grouped from, and cutting a release |

## License

[MIT](LICENSE).

# Reproducible CI

Pin transitive collections with a lockfile, then drive CI from it:

```bash
# once, when you change the requirements file (galaxy.toml or requirements.yml):
go-galaxy lock             # writes galaxy.lock

# in CI:
go-galaxy install --frozen # install exactly the locked versions
```

A frozen install never installs an artifact that does not match the lockfile's recorded
SHA256, so a poisoned cache or a mutated upstream artifact cannot be installed: a cached
artifact that does not match is evicted and downloaded again once (never under
`--offline`), and a mismatch that is still there fails the run (exit `7`). A Galaxy entry
with no recorded SHA (its server publishes none) is not pin-checked. On a cold cache a
frozen install downloads each Galaxy artifact from the `download_url` the lockfile
records, asking the server for no metadata at all unless it verifies signatures, so a
fresh runner spends its requests on the artifacts alone. A git or Galaxy role is pinned by
commit rather than by digest: a frozen install serves it from the cache, and on a miss
fetches exactly the pinned commit and refuses a repository that serves another one (exit
`7`); a url role is pinned by its origin bytes' SHA256 and refused the same way when the
URL serves other bytes.

`--frozen` decides *what* gets installed and needs no cache to do it. `--offline`
is a separate, stronger promise: no network call at all, so an artifact that is
not already cached is not a download but a failure. Against a cold cache the run
exits `5` naming the collection it could not get. Add `--offline` only where the
cache is known to be populated - a base image you baked it into ([container image
bake](#container-image-bake) below), or a restored CI cache your job treats as
mandatory. A restored CI cache is not that by default: the first run after any
lockfile change misses by construction, because the key just changed.

`go-galaxy hash` prints a deterministic `sha256:…` of the lockfile (or of the
requirements file, `galaxy.toml` or `requirements.yml`, when no lockfile is
present) - perfect as a CI cache key. The lockfile it looks for is
`--lock-file` (or `GO_GALAXY_LOCK_FILE`) when set, else, with `galaxy.toml`,
the one `[tool.go-galaxy] lock_file` names, else `galaxy.lock` beside the
requirements file. Reading that one key means decoding the file and expanding
every `${VAR}` under `[tool.go-galaxy]`: a `galaxy.toml` that is not TOML,
breaks the schema or names a variable the job did not export exits `2` rather
than yielding a key, where a `requirements.yml` that is not YAML still hashes
as the bytes it is.

**Upgrade note (after v1.2.3):** the lockfile is now written with a two-space
indent instead of yaml's default four. The hash is computed over the file as
`lock` writes it, so upgrading past v1.2.3 changes `go-galaxy hash` (and the
metrics report's `lockfile_hash`) for every lockfile once, and the first CI run
after it misses the cache. An existing four-space file still loads and still
passes `--frozen` unchanged; the next plain `lock` rewrites it with the new
indent, which shows as a whitespace-only diff. Both binaries read either
layout, but they print different hashes for the same file, so jobs sharing a
cache key must run the same release. Adopting `galaxy.toml` is the same kind
of change: a release that predates it still reads `requirements.yml`, so a
checkout holding only `galaxy.toml` fails it, and one holding both hashes a
different file whenever no lockfile is present, so the jobs sharing a cache
key have to move together.

## Roles

A `roles:` list in the same requirements file (`galaxy.toml` or `requirements.yml`)
is installed by the same `install`, locked by the same `lock` and warmed by the
same `warm`; nothing in the jobs below
changes for it except where the roles land and how the playbook step finds them.
Roles install under `--roles-path` (`GO_GALAXY_ROLES_PATH`, `ANSIBLE_ROLES_PATH`,
`[defaults] roles_path`), `.roles` beside the working directory by default, so a
later `ansible-playbook` step needs the same directory on its search path - set
`ANSIBLE_ROLES_PATH` once for the job and both tools read it, exactly as
`ANSIBLE_COLLECTIONS_PATH` serves the collections:

```yaml
env:
  ANSIBLE_COLLECTIONS_PATH: ./collections
  ANSIBLE_ROLES_PATH: ./roles

steps:
  - run: go-galaxy install --frozen
  - run: ansible-playbook site.yml
```

A lockfile that holds a Galaxy collection is written as `schema_version: 5`,
since each Galaxy entry carries its `download_url`; one without that holds a
url source (a collection's or a role's) as `schema_version: 4`, and one
holding a role as `schema_version: 3`. A go-galaxy binary predating those
features refuses such a file (exit `6`) rather than installing what it
understands and silently skipping the rest; pin the binary version across the
jobs that share the lockfile. The reverse holds for schema 5: a lockfile an
older release wrote carries no `download_url`, so this one refuses it (exit
`6`) until `go-galaxy lock` rewrites it.

## GitHub Actions

This repository publishes a composite action that does the whole of it -
download, checksum check, cache key, restore, install, save:

```yaml
name: ansible-collections
on: [push, pull_request]

jobs:
  install:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

      - uses: greeddj/go-galaxy@v1
        with:
          collections-path: ./collections
          frozen: true
```

It downloads the release binary, checks it against the release's own
`checksums.txt`, and refuses to go on if that file names no such asset. Then it
computes `go-galaxy hash` over your requirements, restores `~/.cache/go-galaxy`
under that key, installs, and saves the cache on the way out.

Reference it by exact release, `greeddj/go-galaxy@v1.2.0`, or by the major tag
`@v1`, which moves with each release. The reference also decides which
go-galaxy is installed: an exact one installs that release, while `@v1` and a
branch install the latest, and the `version` input overrides either.

Linux and macOS runners only - those are the platforms the release builds for,
and any other is refused by name rather than left to fail on a download.

| Input | Default | What it does |
| :-- | :-- | :-- |
| `requirements` | go-galaxy's default | `-r`; the cache-key step hashes the same file the install reads |
| `collections-path` | go-galaxy's default | `-p` |
| `roles-path` | go-galaxy's default | `--roles-path` |
| `frozen` | `false` | Install exactly what the lockfile pins |
| `offline` | `false` | Make no network call; an uncached artifact is a failure |
| `args` | empty | Extra arguments, split on whitespace |
| `install` | `true` | `false` puts go-galaxy on PATH and stops |
| `cache` | `true` | Restore and save `~/.cache/go-galaxy` |
| `version` | from the reference | The release to install, 1.1.0 or later |

Every input adds its flag only when set, so an input you leave alone leaves the
matching `GO_GALAXY_*` variable in charge rather than silently outranking it.

### Everything else, through the environment

The action covers the common flags and nothing more. The rest of the surface -
`GO_GALAXY_TOKEN`, the per-host git and url credentials, the `ANSIBLE_*`
settings in [Configuration](configuration.md) - reaches go-galaxy through the
environment, and a composite action inherits it: workflow-level `env`,
job-level `env`, and `env` on the step that calls the action all reach it.

```yaml
      - uses: greeddj/go-galaxy@v1
        env:
          GO_GALAXY_TOKEN: ${{ secrets.GALAXY_TOKEN }}
          GO_GALAXY_GIT_HUB_URL: https://github.com/acme/
          GO_GALAXY_GIT_HUB_USERNAME: x-access-token
          GO_GALAXY_GIT_HUB_PASSWORD: ${{ secrets.GH_PAT }}
        with:
          frozen: true
          args: --required-valid-signature-count 1
```

**Put no secret in `args`.** That input becomes argv, and argv is readable by
any other process on the runner for the life of the run; the environment route
is the one this tool documents for every secret it takes, and the reason
`--token` exists only for interactive use. See
[Security](security.md#security--trust-model).

### Settings in galaxy.toml

A project on `galaxy.toml` can keep some of that surface in the file instead
of the workflow, under `[tool.go-galaxy]`: the S3 bucket and endpoint in an
`s3` table, with the two keys written as `${VAR}` references rather than
literals, and the Galaxy servers as a `[[tool.go-galaxy.servers]]` list in
place of the `[galaxy_server.*]` sections of an `ansible.cfg`. Every `${VAR}`
under `[tool.go-galaxy]` is expanded from the environment go-galaxy runs in,
a flag or `GO_GALAXY_*` variable that is set still outranks the file key by
key, and a reference to a variable that is not exported fails the run with
exit `2`, naming the variable and never a value:

```toml
[tool.go-galaxy.s3]
bucket = "ci-galaxy-cache"
endpoint = "https://s3.example.com"
access_key = "${S3_CACHE_ACCESS_KEY}"
secret_key = "${S3_CACHE_SECRET_KEY}"

[[tool.go-galaxy.servers]]
id = "hub"
url = "https://hub.example.com/api/galaxy/"
token = "${HUB_TOKEN}"
```

```yaml
      - uses: greeddj/go-galaxy@v1
        env:
          S3_CACHE_ACCESS_KEY: ${{ secrets.S3_CACHE_ACCESS_KEY }}
          S3_CACHE_SECRET_KEY: ${{ secrets.S3_CACHE_SECRET_KEY }}
          HUB_TOKEN: ${{ secrets.HUB_TOKEN }}
          ANSIBLE_GALAXY_SERVER_HUB_URL: https://hub.example.com/api/galaxy/
        with:
          frozen: true
```

The action's cache-key step runs `go-galaxy hash` over that same file and
inherits the same environment as its install step, so a variable the file
names has to be exported where both steps see it - the workflow, the job or
the step that calls the action, as above - and one that is not set fails the
key step with exit `2` before any cache is restored. The same holds for the
by-hand workflow [below](#without-the-action), whose `Compute cache key` step
is a step of its own: a variable set on the install step alone is unset when
`hash` reads the file, so export it on the job.

`ANSIBLE_GALAXY_SERVER_HUB_URL` is there for the same reason it would be
beside an `ansible.cfg` section. A `token` spelled as `${VAR}` is your token,
not the file's, and a checked-out file must not pick where your token goes,
so a `${VAR}` token paired with the `url` the file wrote is refused (exit `2`)
until the address is on your channel too: export
`ANSIBLE_GALAXY_SERVER_<ID>_URL` with the same address (and
`_VALIDATE_CERTS`, when the entry disables certificate checks). A token
written into the file as a literal is the file's own and is not refused - not
recommended, since the file is committed. See [Galaxy servers and
authentication](servers-and-auth.md#--token).

Outputs are `version`, the release actually installed, and `cache-hit`.

Three of those are worth a sentence. `install: false` is for a job that drives
go-galaxy itself - several commands, or `lock` and `outdated` rather than
`install` - and wants only the binary on PATH. Set `cache: false` if the job
sets `GO_GALAXY_CACHE_DIR` itself, or its `galaxy.toml` names a `cache_dir`,
because the action caches the default path and would otherwise save an empty
one. And `version` will not go below 1.1.0,
which is the first release to publish the raw per-platform binaries the action
fetches; earlier releases shipped archives only.

The cache key carries the go-galaxy release alongside the runner platform and
`go-galaxy hash`, so one repository can pin one workflow to `@v1.2.0` and let
another track `@v1` without the two sharing a cache. They must not: the cache
snapshot is versioned, and a binary handed a snapshot from a newer one refuses
it rather than rebuilding. The price is a cold run on the first job after an
upgrade.

When the action installs the latest release (`@v1` or a branch, with no
`version` input), it learns which release it got from the first word of
`go-galaxy --version`: the version, starting with a digit and with any leading
`v` dropped. That word becomes both the `version` output and the release in the
cache key, so `--version` has to keep printing the version first. Output that
does not start that way leaves `version` empty and gives every release one
shared cache key - the very case the release in the key exists to prevent.

### Without the action

The same thing by hand, for a runner the action does not cover or a pipeline
that wants every step visible:

```yaml
      - name: Install go-galaxy
        run: |
          curl -sSL https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64 \
            -o /usr/local/bin/go-galaxy
          chmod +x /usr/local/bin/go-galaxy

      - name: Compute cache key
        id: gg
        run: echo "key=$(go-galaxy hash)" >> "$GITHUB_OUTPUT"

      - name: Restore go-galaxy cache
        uses: actions/cache@v4
        with:
          path: ~/.cache/go-galaxy
          key: go-galaxy-${{ runner.os }}-${{ steps.gg.outputs.key }}
          restore-keys: |
            go-galaxy-${{ runner.os }}-

      # --frozen, not --frozen --offline: restore-keys can hand this job a
      # cache from an older lockfile, and the run after a lockfile change gets
      # no hit at all. Offline would make either a failure instead of a
      # download; frozen alone still installs exactly what the lockfile pins.
      - name: Install collections (frozen)
        run: go-galaxy install --frozen -p ./collections
```

## GitLab CI

```yaml
stages: [install]

variables:
  GO_GALAXY_CACHE_DIR: "$CI_PROJECT_DIR/.cache/go-galaxy"

install_collections:
  stage: install
  # Not ghcr.io/greeddj/go-galaxy: that image is distroless and carries no
  # shell, and GitLab runs every job's script through one, so a job in it
  # cannot start at all. Drop the static binary into an ordinary image.
  image: alpine:3
  cache:
    # cache:key is expanded when the job is created, and the cache is restored
    # before before_script runs, so a key computed by a script step is always
    # too late: whatever it expands to is the same for every pipeline, which
    # means one shared cache entry rather than one per lockfile. cache:key:files
    # makes GitLab hash the lockfile itself - the same input `go-galaxy hash`
    # reads.
    key:
      files:
        - galaxy.lock
      prefix: go-galaxy
    paths:
      - .cache/go-galaxy
  before_script:
    - apk add --no-cache ca-certificates curl
    - curl -sSLf -o /usr/local/bin/go-galaxy https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
    - chmod +x /usr/local/bin/go-galaxy
  script:
    # --frozen without --offline: a cache miss is normal here - the first
    # pipeline after a lockfile change gets one - and --offline would turn it
    # into a failed job instead of a download.
    - go-galaxy install --frozen -p ./collections
```

**Distributed runners.** GitLab's own `cache:` is per-runner unless the runner
is configured with a distributed cache, so with several runners each one
rebuilds its own copy. Pointing go-galaxy at its own S3 cache instead gives
every runner one shared artifact cache: set `GO_GALAXY_S3_BUCKET`,
`GO_GALAXY_S3_REGION` and `GO_GALAXY_S3_PREFIX` in `variables:`, and
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` as masked project variables,
which go-galaxy reads directly. A project on `galaxy.toml` can keep the
bucket, region, prefix and endpoint in the file instead, under
`[tool.go-galaxy.s3]`, with the two keys written as `${VAR}` references to the
masked variables: a `GO_GALAXY_S3_*` variable that is set still outranks the
file key by key, no key enters the repository as a literal, and a reference to
a variable the job does not export fails the run with exit `2` naming it. Keep
the `cache:` block alongside either: the extracted-tree store stays local to
`GO_GALAXY_CACHE_DIR` even with the S3 backend, so the job cache is what saves
re-extracting every collection. Such a job cannot also run `--offline`: the
bucket, from a variable or from `galaxy.toml`, is reached over the network, so
the pair exits `2` before the cache is opened.

Two runtime consequences of a shared S3 cache are worth knowing before you
enable it. Jobs sharing one bucket serialize: a run holds the backend's
exclusive lock for its whole duration, so a `parallel:` matrix against one
bucket runs one job at a time, and a job that gives up waiting on another's
lock exits `8`. And a bucket is a trust boundary, not just storage: give jobs
that build untrusted branches or forks their own bucket, and see [Security /
Trust model](security.md#security--trust-model) for why a prefix alone is not a boundary.

## Lockfile drift gate

Fail a pull request when `galaxy.lock` no longer matches the requirements
file (`galaxy.toml` or `requirements.yml`) - a root added, removed, or
repinned without regenerating
the lockfile. `lock --check` reads the lockfile as the thing to check rather
than as the answer, which is the opposite of what install/warm `--frozen` do -
it still resolves fresh, and only a warm resolve cache lets that stay off the
network - so this is a separate job from the install above, not a replacement
for it. Add
`--refresh` for a second, distinct gate on the same file: `lock --check`
alone only catches a change to the requirements file (`galaxy.toml` or
`requirements.yml`), since it reuses the cached
resolve; `lock --check --refresh` also catches a newer version simply
having been published upstream, since `--refresh` makes the comparison's
fresh resolve reach the live servers instead:

```yaml
name: lockfile-drift
on: [pull_request]

jobs:
  lockfile-drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install go-galaxy
        run: |
          curl -sSL https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64 \
            -o /usr/local/bin/go-galaxy
          chmod +x /usr/local/bin/go-galaxy

      - name: Check galaxy.lock matches the requirements file
        run: go-galaxy lock --check

      - name: Check galaxy.lock is not stale against upstream
        run: go-galaxy lock --check --refresh
```

A nonzero exit from either step (code `6`, the lockfile class - see
[Exit codes](exit-codes.md)) means the PR needs `go-galaxy lock` (optionally
`--refresh`, to pick up the newer upstream version too) run and its updated
`galaxy.lock` committed.

## Container image bake

Pre-warm caches in your CI base image so jobs only hardlink into place:

```dockerfile
FROM debian:stable-slim

# The published image is distroless: one static binary at /go-galaxy, no shell
# and no package manager. A COPY --from takes that binary and nothing else, so
# the CA certificates it needs have to come from this base image - install them
# here, or the first Galaxy request fails to verify its TLS certificate.
COPY --from=ghcr.io/greeddj/go-galaxy:latest /go-galaxy /usr/local/bin/go-galaxy
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Pin the cache somewhere that does not depend on who runs the job: the
# default is $HOME/.cache/go-galaxy, and the warm below runs as root while
# your jobs may not.
ENV GO_GALAXY_CACHE_DIR=/var/cache/go-galaxy

WORKDIR /src
# For a project on galaxy.toml: COPY galaxy.toml galaxy.lock ./
COPY requirements.yml galaxy.lock ./
# The uid your jobs run as. A run needs the cache lock and writes the
# snapshot back, so a job user that can only read the baked cache fails to
# start with `cache backend cannot be used as configured` (exit 2) - and so
# does a job whose uid is not the one named here.
ARG JOB_UID=1001
RUN go-galaxy warm --frozen && chown -R "$JOB_UID" "$GO_GALAXY_CACHE_DIR"
```

`chown`, not `chmod -R a+rwX`: an install hardlinks its files out of the cache,
so a cache file and the installed file that came from it are one inode - see
[Differences a migration runs into](ansible-galaxy-compat.md#differences-a-migration-runs-into) for what
that costs an installed file's mode. Widening the cache's modes far enough for a
job to link out of it is therefore the same act as making every installed file
writable, and an edit to one of those installed files lands back in the shared
cache for every later job built on that image. Ownership sidesteps that: with
`fs.protected_hardlinks` set, the default on current distributions, Linux
permits a hardlink to a file you do not own only when you may also write it,
while a file you own you may always link - so handing the cache to the job's uid
buys the link while leaving an installed file read-only and the cache writable
by nothing but the job. That uid can still `chmod u+w` a cache file it owns and
edit it, which is the ordinary standing of any single-user cache. Keep the
`chown` chained onto the same `RUN`: a separate one rewrites every file's
metadata into a new layer and copies the whole cache again. Where the job's uid
cannot be known at build time, no setting keeps both properties - read-only
cache files cost the hardlink, so jobs copy the bytes instead and a collection
whose extracted tree is not already in the image fails outright (exit `5`),
while `chmod -R a+rwX` keeps the hardlink and gives up both the read-only
installed file and the private cache. Bake for a known uid where you can.

Jobs built on that image are the case `--offline` is for, since the cache is
part of the image rather than something a key might miss:

```bash
go-galaxy install --frozen --offline -p ./collections
```

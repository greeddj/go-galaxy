# Quick start

Install go-galaxy, install a project's collections and roles, pin their
versions, and run the same install in CI.

## Install

=== "Homebrew"

    ```bash
    brew install --cask greeddj/tap/go-galaxy
    ```

    Works on macOS and Linux. The builds are not notarized by Apple, so on
    macOS the cask clears their quarantine attribute and Gatekeeper does not
    check them. To check the download yourself, follow
    [Verifying a release](../guides/security.md#verifying-a-release).

=== "go install"

    ```bash
    go install github.com/greeddj/go-galaxy/cmd/go-galaxy@latest
    ```

    It installs into `$(go env GOPATH)/bin`. Put that directory on `PATH`.

=== "Binary"

    ```bash
    base=https://github.com/greeddj/go-galaxy/releases/latest/download
    curl -sSLf -O "$base/go-galaxy-linux-amd64" -O "$base/checksums.txt"
    sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
    sudo mkdir -p /usr/local/bin
    sudo install -m 755 go-galaxy-linux-amd64 /usr/local/bin/go-galaxy
    ```

    Swap in `linux-arm64`, `darwin-amd64` or `darwin-arm64` for `linux-amd64`.
    The darwin builds need macOS 13 or later.
    [Verifying a release](../guides/security.md#verifying-a-release) adds the
    signature check of `checksums.txt`.

=== "Container"

    ```bash
    alias go-galaxy='docker run --rm -u "$(id -u):$(id -g)" -e HOME=/work -v "$PWD":/work -w /work ghcr.io/greeddj/go-galaxy'
    ```

    With this alias, the steps below work as written. The image runs go-galaxy
    as the unprivileged user `65532`. `-u` runs it as you instead, so it can
    write into your project. `HOME` puts its cache in `.cache` there, so add
    `.cache/` to `.gitignore`.

    | Tag | Base | Use |
    | :-- | :-- | :-- |
    | `latest` (never a prerelease), `<version>`, `<version>-distroless` | distroless, no shell | the alias above, or [baking into a CI image](../guides/ci.md#container-image-bake) |
    | `<version>-alpine` | Alpine, with a shell | a CI job that runs go-galaxy from its script, as in [GitLab CI](../guides/ci.md#gitlab-ci) |

=== "GitHub Actions"

    ```yaml
    - uses: greeddj/go-galaxy@v1.3.0
    ```

    The job is under [Run it in CI](#run-it-in-ci).

=== "GitLab CI"

    The job runs the `<version>-alpine` image, which has a shell for its
    script. The job is under [Run it in CI](#run-it-in-ci).

> [!NOTE]
> Already using ansible-galaxy? Your `requirements.yml`, `ansible.cfg` and
> `ANSIBLE_*` variables mostly carry over. Some differences break a pipeline
> that worked under `ansible-galaxy`, so read
> [Differences a migration runs into](ansible-galaxy-compat.md#differences-a-migration-runs-into)
> before you switch.

## Your first install

=== "galaxy.toml"

    ```toml
    [project]
    collections = [
      "community.general >= 10.0.0", # (1)!
      "ansible.utils",
    ]
    roles = ["geerlingguy.docker"]
    ```

    1.  The constraint after the name is optional.
        [Version constraints](../guides/requirements.md#version-constraints) has the
        grammar.

=== "requirements.yml"

    ```yaml
    ---
    collections:
      - name: community.general
        version: ">=10.0.0" # (1)!
      - name: ansible.utils
    roles:
      - name: geerlingguy.docker
    ```

    1.  Optional. [Version constraints](../guides/requirements.md#version-constraints)
        has the grammar.

Save either file in the project root, then run `go-galaxy install` there:

```text
$ go-galaxy install
· Resolve dependencies
· Resolve roles
✔ Installed: community.library_inventory_filtering_v1 == 1.1.5
✔ Installed: ansible.utils == 6.1.1
✔ Installed: community.general == 13.4.0
✔ Installed: role geerlingguy.docker == 8.0.0
✔ All done. Took 26s
```

```text title="Installed tree"
.collections/ansible_collections/ansible/utils/
.collections/ansible_collections/community/general/
.collections/ansible_collections/community/library_inventory_filtering_v1/
.roles/geerlingguy.docker/
```

go-galaxy [reads](../guides/requirements.md#which-file-is-read) `./galaxy.toml` when
present, else `./requirements.yml`. It installs dependencies too, such as
`community.library_inventory_filtering_v1`, and [caches](../guides/caching.md) each
artifact in `~/.cache/go-galaxy`. Add `.collections/` and `.roles/` to
`.gitignore`.

### Point ansible at the installs

ansible-playbook does not look in `.collections` or `.roles` by default.
Point it there in one of two ways:

- Export `ANSIBLE_COLLECTIONS_PATH=.collections` and
  `ANSIBLE_ROLES_PATH=.roles`. In CI, set them on the job.
- Set `collections_path = .collections` and `roles_path = .roles` under
  `[defaults]` in `ansible.cfg`.

go-galaxy installs where these point, so one setting serves both tools.

## Pin the versions

```bash
go-galaxy lock               # (1)!
git add galaxy.lock          # (2)!
go-galaxy install --frozen   # (3)!
```

1.  Resolves every collection and role, dependencies included, and writes
    `galaxy.lock` beside the requirements file.
2.  Commit it, and rerun `go-galaxy lock` whenever you edit the requirements.
3.  Installs exactly what the lockfile pins, without resolving. A missing
    lockfile, or one that no longer covers the requirements, fails with exit
    [`6`](../reference/exit-codes.md). Entries the requirements no longer need
    still install. [`lock --check`](../guides/lockfile.md#catch-drift) catches
    those.

```text
$ go-galaxy lock
✔ Lockfile written to galaxy.lock (3 collections, 1 roles)
```

The [lockfile](../guides/lockfile.md#what-each-entry-is-pinned-by) pins each collection
to its version, download URL and sha256, and the role to a git commit.

## Run it in CI

=== "GitHub Actions"

    ```yaml title=".github/workflows/ansible-deps.yml"
    on: [push, pull_request]
    jobs:
      install:
        runs-on: ubuntu-latest
        steps:
          - uses: actions/checkout@v7
          - uses: greeddj/go-galaxy@v1.3.0 # (1)!
            with:
              frozen: true
    ```

    1.  Pin the release that writes your `galaxy.lock`, and bump it when you
        relock with a newer one ([Pin one release](../guides/ci.md#pin-one-release)).

    On Linux and macOS runners, the action installs a checksum-verified
    binary, caches `~/.cache/go-galaxy` under a key that includes
    [`go-galaxy hash`](../guides/lockfile.md#a-cache-key-for-ci) and runs
    `go-galaxy install --frozen`.

=== "GitLab CI"

    ```yaml title=".gitlab-ci.yml"
    variables:
      GO_GALAXY_CACHE_DIR: "$CI_PROJECT_DIR/.cache/go-galaxy"

    install:
      image:
        name: ghcr.io/greeddj/go-galaxy:1.3.0-alpine # (1)!
        entrypoint: [""]
      cache:
        key:
          files: [galaxy.lock]
          prefix: go-galaxy-1.3.0
        paths: [.cache/go-galaxy]
      script:
        - go-galaxy install --frozen
    ```

    1.  Pin the release that writes your `galaxy.lock`, here and in the cache
        `prefix`. Bump both when you relock with a newer one
        ([Pin one release](../guides/ci.md#pin-one-release)).

    The job keeps its cache inside the project directory, the only place
    GitLab caches, under a key built from `galaxy.lock` and the release. A
    later playbook job does not see the installed `.collections` and `.roles`.
    [GitLab CI](../guides/ci.md#gitlab-ci) shows how to hand them on.

[CI pipelines](../guides/ci.md) has the rest.

## If a run fails

| Exit | Likely cause | Read |
| :-- | :-- | :-- |
| `2` | A missing or invalid requirements file, flag or setting | [Requirements files](../guides/requirements.md), [Configuration](../reference/configuration.md) |
| `3` | No version fits the constraints, or a collection, role, version or ref does not exist | [When no version fits](../guides/requirements.md#when-no-version-fits) |
| `4` | While resolving, a server is unavailable, times out, answers an error, or rejects the credentials | [Servers and credentials](../guides/servers-and-auth.md) |
| `5` | A collection or role failed to download or install. Its `Failed:` line says why | [When several things fail](../reference/exit-codes.md#when-several-things-fail) |
| `6` | `galaxy.lock` is missing, invalid, or does not cover a requirement | [Lockfile](../guides/lockfile.md) |
| Any other | See the full table | [Exit codes](../reference/exit-codes.md) |

Rerun with `--verbose` to log the servers, the settings `galaxy.toml` or
`ansible.cfg` supplied, each metadata request and step timings.

## Next steps

<div class="grid cards" markdown>

-   :lucide-arrow-right-left: **[Coming from ansible-galaxy](ansible-galaxy-compat.md)**

    ---

    What carries over, and what a migration runs into.

-   :lucide-lock: **[Lockfile](../guides/lockfile.md)**

    ---

    Catch drift, inspect what is locked, and find newer versions.

-   :lucide-workflow: **[CI pipelines](../guides/ci.md)**

    ---

    Action inputs, GitLab CI, a drift gate and a baked image.

-   :lucide-key-round: **[Servers and credentials](../guides/servers-and-auth.md)**

    ---

    Private hubs, tokens, TLS, and git and URL credentials.

</div>

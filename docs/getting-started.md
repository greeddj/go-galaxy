# Get started

Install go-galaxy, install a project's collections and roles, pin their
versions, and run the same install in CI.

## Install

=== "Homebrew"

    ```bash
    brew install --cask greeddj/tap/go-galaxy
    ```

    Works on macOS and Linux. On macOS it clears the quarantine flag of these
    unnotarized builds, skipping Gatekeeper;
    [verify the release](security.md#verifying-a-release) yourself.

=== "go install"

    ```bash
    go install github.com/greeddj/go-galaxy/cmd/go-galaxy@latest
    ```

    It installs into `$(go env GOPATH)/bin`; put that on `PATH`.

=== "Binary"

    ```bash
    sudo curl -sSLf --create-dirs -o /usr/local/bin/go-galaxy \
      https://github.com/greeddj/go-galaxy/releases/latest/download/go-galaxy-linux-amd64
    sudo chmod +x /usr/local/bin/go-galaxy
    ```

    Swap in `linux-arm64`, `darwin-amd64` or `darwin-arm64`; darwin needs
    macOS 13 or later. [Verify it](security.md#verifying-a-release) before use.

=== "Container"

    ```bash
    docker run --rm ghcr.io/greeddj/go-galaxy --version
    ```

    Meant for [baking into a CI image](ci.md#container-image-bake); the steps
    below need a local binary.

=== "GitHub Actions"

    ```yaml
    - uses: greeddj/go-galaxy@v1
    ```

    The job is under [Run it in CI](#run-it-in-ci).

> [!NOTE]
> Already using ansible-galaxy? Your `requirements.yml`, and the `ansible.cfg`
> keys and `ANSIBLE_*` variables for paths and servers, carry over;
> [Coming from ansible-galaxy](ansible-galaxy-compat.md) lists what differs.

## Your first install

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

    1.  Optional. [Version constraints](requirements.md#version-constraints)
        has the grammar.

=== "galaxy.toml"

    ```toml
    [project]
    collections = [
      "community.general >= 10.0.0",
      "ansible.utils",
    ]
    roles = ["geerlingguy.docker"]
    ```

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

go-galaxy [reads](requirements.md#which-file-is-read) `./galaxy.toml` when
present, else `./requirements.yml`. It installs dependencies too, such as
`community.library_inventory_filtering_v1`, and [caches](caching.md) each
artifact in `~/.cache/go-galaxy`. Add `.collections/` and `.roles/` to
`.gitignore`.

> [!TIP]
> ansible-playbook does not look in `.collections` or `.roles` by default.
> Set `ANSIBLE_COLLECTIONS_PATH=.collections` and `ANSIBLE_ROLES_PATH=.roles`
> (in CI, at job level), or `collections_path` and `roles_path` under
> `[defaults]` in `ansible.cfg`; go-galaxy reads them too.

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
    [`6`](exit-codes.md); [`lock --check`](lockfile.md#catch-drift) catches
    other drift.

```text
$ go-galaxy lock
✔ Lockfile written to galaxy.lock (3 collections, 1 roles)
```

The [lockfile](lockfile.md#what-each-entry-is-pinned-by) pins each collection
to a version and sha256, and the role to a git commit.

## Run it in CI

```yaml title=".github/workflows/ansible-deps.yml"
on: [push, pull_request]
jobs:
  install:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: greeddj/go-galaxy@v1
        with:
          frozen: true
```

On Linux and macOS runners, the action installs a checksum-verified binary,
caches `~/.cache/go-galaxy` under a key that includes
[`go-galaxy hash`](lockfile.md#a-cache-key-for-ci) and runs
`go-galaxy install --frozen`; [CI pipelines](ci.md) has the rest.

## If a run fails

| Exit | Likely cause | Read |
| :-- | :-- | :-- |
| `2` | A missing or invalid requirements file, flag or setting | [Requirements files](requirements.md), [Configuration](configuration.md) |
| `3` | No version fits the constraints, or a collection, role, version or ref does not exist | [When no version fits](requirements.md#when-no-version-fits) |
| `4` | A server is unavailable, times out, answers an error, or rejects the credentials | [Servers and credentials](servers-and-auth.md) |
| `6` | `galaxy.lock` is missing, invalid, or does not cover a requirement | [Lockfile](lockfile.md) |
| Any other | See the full table | [Exit codes](exit-codes.md) |

Rerun with `--verbose` to log the servers, the settings `galaxy.toml` or
`ansible.cfg` supplied, each metadata request and step timings.

## Next steps

<div class="grid cards" markdown>

-   :lucide-arrow-right-left: **[Coming from ansible-galaxy](ansible-galaxy-compat.md)**

    ---

    What carries over, and what a migration runs into.

-   :lucide-lock: **[Lockfile](lockfile.md)**

    ---

    Catch drift, inspect what is locked, and find newer versions.

-   :lucide-workflow: **[CI pipelines](ci.md)**

    ---

    Action inputs, GitLab CI, a drift gate and a baked image.

-   :lucide-key-round: **[Servers and credentials](servers-and-auth.md)**

    ---

    Private hubs, tokens, TLS, and git and URL credentials.

</div>

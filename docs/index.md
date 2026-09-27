<div class="gg-hero" markdown>

![](assets/logo.svg#only-light)
![](assets/logo-white.svg#only-dark)

# go-galaxy

</div>

Fast Ansible Galaxy collections and roles installer for CI.

CI pipelines often spend minutes downloading and unpacking Galaxy collections
and roles. go-galaxy resolves, downloads and extracts them in parallel,
hardlinks out of a content-addressed cache on warm runs, and skips the network
entirely under `--frozen --offline`. With a warm cache, installing 100
collections takes seconds rather than minutes.

![go-galaxy against ansible-galaxy, install speedup by cache state and collection count](assets/benchmark.svg)

*Mean of 5 runs on Linux, both tools with `--no-deps`. The warm rows compare
[two cache designs](reference/benchmarks.md#what-each-tool-caches).*

## Why go-galaxy

- **Shared cache.** An optional [S3 bucket](guides/caching.md#s3-cache-optional)
  shares downloads across runners, and [`cleanup`](guides/caching.md#clearing-and-cleanup)
  removes what no project uses any more.
- **Reproducible.** `galaxy.lock`
  [pins](guides/lockfile.md#what-each-entry-is-pinned-by) every collection and
  role to an exact version, commit or sha256.
- **Refuses rather than guesses.** A PubGrub resolver backtracks to older
  releases until the constraints fit, or prints a
  [proof](guides/requirements.md#when-no-version-fits) that none do and exits
  [`3`](reference/exit-codes.md).
- **Reads your ansible-galaxy setup.** go-galaxy takes `requirements.yml` and
  the `ansible.cfg` keys and `ANSIBLE_*` variables for paths, servers and
  timeouts. It adds its own [`galaxy.toml`](guides/requirements.md#galaxytoml).
- **Many sources.** Collections and roles come from Galaxy, git and http(s)
  tarballs. With a [keyring](guides/signatures.md), Galaxy collection
  signatures are checked in pure Go, without `gpg`.

## Start here

<div class="grid cards" markdown>

-   :lucide-rocket: **[Quick start](get-started/getting-started.md)**

    ---

    Install the binary, run a first install, pin it for CI.

-   :lucide-arrow-right-left: **[Coming from ansible-galaxy](get-started/ansible-galaxy-compat.md)**

    ---

    What stays the same, and what a migration runs into.

-   :lucide-file-text: **[Requirements files](guides/requirements.md)**

    ---

    Collections and roles from Galaxy, git or a URL.

-   :lucide-lock: **[Lockfile](guides/lockfile.md)**

    ---

    Pin every version, catch drift, inspect what is locked.

-   :lucide-workflow: **[CI pipelines](guides/ci.md)**

    ---

    GitHub Actions, GitLab CI, a drift gate and a baked image.

-   :lucide-key-round: **[Servers and credentials](guides/servers-and-auth.md)**

    ---

    Private hubs, tokens, TLS, and git and URL credentials.

-   :lucide-database: **[Caching and S3](guides/caching.md)**

    ---

    A local cache on each machine, or an S3 bucket every runner shares.

-   :lucide-book-open: **[Reference](reference/cli.md)**

    ---

    Every command and option, plus [configuration](reference/configuration.md),
    [exit codes](reference/exit-codes.md) and [upgrading](reference/upgrading.md).

</div>

## In 30 seconds

```bash
go-galaxy install            # (1)!
go-galaxy lock               # (2)!
go-galaxy install --frozen   # (3)!
```

1.  Reads `-r <file>`, else
    [`./galaxy.toml`, else `./requirements.yml`](guides/requirements.md#which-file-is-read).
    Installs into `.collections` and `.roles` by default. ansible-playbook
    does not look there until you
    [point it at the installs](get-started/getting-started.md#point-ansible-at-the-installs).
2.  Pins every transitive collection and role in `galaxy.lock`. Commit it.
3.  Installs exactly the [locked versions](guides/lockfile.md#install-from-the-lockfile),
    and fails if `galaxy.lock` is missing or no longer covers the requirements.
    Add `--offline` to skip the network only where the cache is known to be
    full.

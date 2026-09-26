<div class="gg-hero" markdown>

![](assets/logo.svg#only-light)
![](assets/logo-white.svg#only-dark)

# go-galaxy

</div>

Fast Ansible Galaxy collections and roles installer for CI.

CI pipelines often spend minutes downloading and unpacking Galaxy collections
and roles. go-galaxy resolves, downloads and extracts them in parallel,
hardlinks out of a content-addressed cache on warm runs, and skips the network
entirely under `--frozen --offline`. With a lockfile and warm caches,
installing 100 collections takes seconds rather than minutes.

![go-galaxy against ansible-galaxy, install speedup by cache state and collection count](assets/benchmark.svg)

*Mean of 5 runs on Linux, both tools with `--no-deps`; the warm rows compare
[two cache designs](reference/benchmarks.md#what-each-tool-caches).*

## Start here

<div class="grid cards" markdown>

-   :lucide-rocket: **[Get started](get-started/getting-started.md)**

    ---

    Install the binary, run a first install, pin it for CI.

-   :lucide-arrow-right-left: **[Coming from ansible-galaxy](get-started/ansible-galaxy-compat.md)**

    ---

    What stays the same, and what a migration runs into.

-   :lucide-file-text: **[Requirements files](guides/requirements.md)**

    ---

    Collections and roles from Galaxy, git or a URL.

-   :lucide-lock: **[Lockfile and CI](guides/lockfile.md)**

    ---

    Pin every version, then install exactly those in [CI](guides/ci.md).

-   :lucide-key-round: **[Servers and credentials](guides/servers-and-auth.md)**

    ---

    Private hubs, tokens, TLS, and git and URL credentials.

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
    Installs into `.collections` and `.roles` by default;
    [point ansible there](get-started/getting-started.md#your-first-install).
2.  Pins every transitive collection and role in `galaxy.lock`. Commit it.
3.  Installs exactly the [locked versions](guides/lockfile.md#install-from-the-lockfile),
    failing if `galaxy.lock` is missing or no longer matches the requirements.
    On a warm local cache, `--offline` skips the network.

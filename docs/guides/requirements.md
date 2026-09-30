# Requirements files

A requirements file lists the collections and roles that `go-galaxy install`
puts in place. Write go-galaxy's own `galaxy.toml` or ansible's
`requirements.yml`: both hold the same two lists.

<div class="grid" markdown>

```toml title="galaxy.toml"
[project]
collections = [
  "community.general >=10.0.0",
  "ansible.utils",
]
roles = ["geerlingguy.docker,8.0.0"]
```

```yaml title="requirements.yml"
---
collections:
  - name: community.general
    version: ">=10.0.0"
  - name: ansible.utils
roles:
  - geerlingguy.docker,8.0.0
```

</div>

| Write | When |
| --- | --- |
| `galaxy.toml` | You want the constraint beside the name, a strict schema, and run settings in [`[tool.go-galaxy]`](../reference/configuration.md#the-toolgo-galaxy-table) |
| `requirements.yml` | `ansible-galaxy` must read the file too |

## Collections

=== "Galaxy"

    === "galaxy.toml"

        ```toml
        [[project.collections]]
        name = "community.general"
        version = ">=10.0.0,<12.0.0" # (1)!
        source = "hub" # (2)!
        ```

        1.  A [constraint](#version-constraints); absent means any version.
            Without `source`, the string `"community.general >=10.0.0,<12.0.0"`
            says the same.
        2.  Optional: the one
            [server to ask](servers-and-auth.md#how-a-collection-picks-its-server).

    === "requirements.yml"

        ```yaml
        collections:
          - name: community.general
            version: ">=10.0.0,<12.0.0" # (1)!
            source: hub # (2)!
        ```

        1.  A [constraint](#version-constraints); absent means any version.
        2.  Optional: the one
            [server to ask](servers-and-auth.md#how-a-collection-picks-its-server).

=== "git"

    === "galaxy.toml"

        ```toml
        [project]
        collections = [
          "git+https://github.com/acme/app.git,v1.2.0", # (1)!
          "git+https://github.com/acme/mono.git#collections/app,main", # (2)!
        ]
        ```

        1.  `,` names the ref: a branch, a tag, `refs/heads/<name>`,
            `refs/tags/<name>` or a full 40-hex commit, never an abbreviated
            one. Absent means `HEAD`. A name that is both a branch and a tag
            means the branch, with a warning.
        2.  `#` picks the directory to build from, the repository root by
            default ([What a git repository must hold](#what-a-git-repository-must-hold)).
            `#` comes before `,`: `<url>,main#sub` asks for the ref `main#sub`.

    === "requirements.yml"

        ```yaml
        collections:
          - name: https://github.com/acme/app.git
            type: git # (1)!
            version: v1.2.0 # (2)!
          - git+https://github.com/acme/mono.git#collections/app,main # (3)!
        ```

        1.  Needed here: a bare `https://` URL is a tarball, not a repository.
        2.  A branch, a tag, `refs/heads/<name>`, `refs/tags/<name>` or a full
            40-hex commit, never an abbreviated one. Absent means `HEAD`. A
            name that is both a branch and a tag means the branch, with a
            warning.
        3.  `#` picks the directory to build from, the repository root by
            default ([What a git repository must hold](#what-a-git-repository-must-hold)).
            `,` names the ref, which wins over a mapping's `version:`. `#`
            comes before `,`: `<url>,main#sub` asks for the ref `main#sub`.

=== "url"

    === "galaxy.toml"

        ```toml
        [project]
        collections = [
          "https://dl.example.com/acme-lib-2.1.0.tar.gz", # (1)!
          { name = "https://dl.example.com/acme-app-1.4.0.tar.gz", version = "1.4.0" }, # (2)!
          "http://cache.example.com/https://dl.example.com/acme-db-1.0.0.tar.gz", # (3)!
        ]
        ```

        1.  Its `MANIFEST.json` names the collection; the downloaded bytes are
            pinned by sha256.
        2.  `version` is an assertion: it must match the manifest, or the run
            exits [`3`](../reference/exit-codes.md).
        3.  A caching proxy: the embedded URL needs a lower-case scheme and
            host, and no default port.

    === "requirements.yml"

        ```yaml
        collections:
          - https://dl.example.com/acme-lib-2.1.0.tar.gz # (1)!
          - name: https://dl.example.com/acme-app-1.4.0.tar.gz
            type: url
            version: "1.4.0" # (2)!
          - http://cache.example.com/https://dl.example.com/acme-db-1.0.0.tar.gz # (3)!
        ```

        1.  Its `MANIFEST.json` names the collection; the downloaded bytes are
            pinned by sha256.
        2.  An assertion: it must match the manifest, or the run exits
            [`3`](../reference/exit-codes.md).
        3.  A caching proxy: the embedded URL needs a lower-case scheme and
            host, and no default port.

| Key | Galaxy | git | url |
| --- | --- | --- | --- |
| `name` | `namespace.name`, each part `^[a-z][a-z0-9_]*$` | Repository URL, or `ns.name` when `source` holds the URL | Tarball URL |
| `namespace` | Beside a one-part `name` | Beside `source` | Refused |
| `version` | Constraint | Ref | Exact version |
| `source` | Server id or URL | Repository URL (optional) | Refused |
| `type` | `galaxy` or absent | `git`, or a `git+` or `git@` name | `url`, or an `http(s)://` name |
| `signatures` | [Allowed](signatures.md#signatures-in-the-requirements-file) | Refused | Refused |

A bare top-level list is read as `collections:`, so an old roles-only file
needs its entries moved under `roles:`.

### What a git repository must hold

The directory `#` picks, the repository root by default, must hold
`galaxy.yml` or `MANIFEST.json`. When it holds neither, every directory one
level down that holds one installs as a collection.

- `galaxy.yml` needs an exact `MAJOR.MINOR.PATCH` version, or the run exits
  `2`.
- A directory holding both `galaxy.yml` and `MANIFEST.json` is refused, exit
  `2`.
- Each `dependencies:` key must be `namespace.name`. A git URL or a path
  exits `3`.
- Two directories declaring one `namespace.name` are refused, exit `2`.
- `build_ignore` is honored. A `manifest:` key is refused, exit `2`.
- Submodules are skipped with a warning.

The internals pages [Git discovery](../internals/install-pipeline.md#git-discovery)
and [Git and url sources](../internals/boundaries.md#git-and-url-sources)
show how the tree is built and checked.

### Version constraints

| Form | Example | Meaning | ansible-galaxy accepts |
| --- | --- | --- | --- |
| Any | `*`, or no version | Every version, prereleases included | Yes, releases only |
| Exact | `1.2.3`, `=1.2.3`, `==1.2.3` | Exactly `1.2.3` | Yes |
| Partial | `1.0` | `>=1.0.0, <1.1.0` | Yes, as exactly `1.0.0` |
| Comparison | `>=1.0`; also `>`, `<`, `<=`, `!=` | As written | Yes |
| AND | `>=2.0, <3.0` or `>=2.0 <3.0` | Both | Comma only |
| OR | <code>^1 &#124;&#124; ^2</code> | Either | No |
| Tilde, caret | `~1.5`, `^1.2` | `>=1.5.0, <1.6.0`; `>=1.2.0, <2.0.0` | No |
| Hyphen range | `1.2 - 1.4` | `1.2.0` through every `1.4.x` | No |
| x-range | `1.x` | `>=1.0.0, <2.0.0` | No |

Every other form admits a prerelease only when it names one, as `>=1.0.0-0`
does ([Prereleases](../get-started/ansible-galaxy-compat.md#prereleases)).

A rerun [replays the last resolution](caching.md#what-a-rerun-reuses), and
[`go-galaxy hash`](lockfile.md#a-cache-key-for-ci) keeps its key, across
spellings of one constraint: whitespace, `,` or a space between AND clauses,
`=`, `==` or nothing before an exact version, `=>`, `=<` and `~>` for `>=`,
`<=` and `~`, and a hyphen range written as its two bounds, `1.2 - 1.4` as
`>=1.2,<=1.4`. Without its spaces, `1.2-1.4` is a prerelease, not a range. Any
other change, such as `1.0` for `1.0.0` or clauses in another order, resolves
again and changes the key.

> [!TIP]
> An unquoted value reads as the text written, so `version: 1.10` asks for
> `1.10` and `version: 1.0` for `1.0.x`. `ansible-galaxy` reads both as
> numbers, `1.10` as `1.1`: quote versions in a file both tools read
> ([Versions and resolution](../get-started/ansible-galaxy-compat.md#versions-and-resolution)).

### When no version fits

=== "galaxy.toml"

    ```toml
    [project]
    collections = [
      "ansible.netcommon >=8.7.0",
      "ansible.utils <3.0.0",
    ]
    ```

=== "requirements.yml"

    ```yaml
    collections:
      - name: ansible.netcommon
        version: ">=8.7.0"
      - name: ansible.utils
        version: "<3.0.0"
    ```

```text
$ go-galaxy install
...
So, because root depends on ansible.netcommon >=8.7.0 and root depends on ansible.utils <3.0.0, version solving failed.
hint: pre-release versions of ansible.netcommon exist and are excluded by plain constraints; if you intended to allow them, use a >=X.Y.Z-0 floor or an exact pin
```

go-galaxy's [PubGrub solver](../internals/solver.md) tries older
releases when a newer one conflicts. When no combination fits, it exits
[`3`](../reference/exit-codes.md). In the proof, `root` is your requirements
file, and the `So, because` line names the constraints to relax. A `hint:`
line about prereleases matters only if you meant to allow them
([Prereleases](../get-started/ansible-galaxy-compat.md#prereleases)).

## Roles

=== "Galaxy role"

    === "galaxy.toml"

        ```toml
        [project]
        roles = [
          "geerlingguy.docker",
          "geerlingguy.nginx,3.2.0,nginx", # (1)!
          { name = "geerlingguy.java", version = "2.3.0" },
        ]
        ```

        1.  `src,version,name`: the name is the directory under `roles_path`.

    === "requirements.yml"

        ```yaml
        roles:
          - geerlingguy.docker
          - geerlingguy.nginx,3.2.0,nginx # (1)!
          - name: geerlingguy.java
            version: "2.3.0"
        ```

        1.  `src,version,name`: the name is the directory under `roles_path`.

=== "git role"

    === "galaxy.toml"

        ```toml
        [project]
        roles = [
          "git+https://git.example.com/platform/ansible-role-base.git,v1.4.0,base",
          "git+https://git.example.com/platform/ansible-role-app.git,release/1.x",
          "https://github.com/acme/ansible-role-cache", # (1)!
        ]
        ```

        1.  A `github.com` URL without `.tar.gz` is a repository, as in ansible.

    === "requirements.yml"

        ```yaml
        roles:
          - git+https://git.example.com/platform/ansible-role-base.git,v1.4.0,base
          - src: https://git.example.com/platform/ansible-role-app.git
            scm: git
            version: release/1.x
          - src: https://github.com/acme/ansible-role-cache # (1)!
        ```

        1.  A `github.com` URL without `.tar.gz` is a repository, as in ansible.

=== "url role"

    === "galaxy.toml"

        ```toml
        [[project.roles]]
        src = "https://dl.example.com/roles/acme-cache-2.0.0.tar.gz"
        name = "cache"
        version = "2.0.0" # (1)!
        ```

        1.  Only a label: the bytes are pinned by sha256.

    === "requirements.yml"

        ```yaml
        roles:
          - src: https://dl.example.com/roles/acme-cache-2.0.0.tar.gz
            name: cache
            version: "2.0.0" # (1)!
        ```

        1.  Only a label: the bytes are pinned by sha256.

| Source | Detected by | `version:` | Default install name |
| --- | --- | --- | --- |
| Galaxy | `owner.role`, each half `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` | A tag; absent or `*` means the highest | `owner.role` |
| git | `git+`, `git@`, `scm: git` or a `github.com` URL | A ref, read as for a [git collection](#collections); absent means `HEAD` | Repository name minus `.git` |
| url | An http(s) URL ending `.tar.gz` | A label; absent means the sha256's first 12 hex digits | File name minus `.tar.gz` |

An install name matches `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$` and is never
`ansible_collections`. A Galaxy role needs a server with the
[v1 role API](servers-and-auth.md#roles-and-the-v1-role-api).

A role needs `meta/main.yml` or `meta/main.yaml` at its top level, not both,
or the run exits `2`. A url role's tarball holds the role at the archive root
or in its single top-level directory.

Each role's `meta` dependencies install too, unless `--no-deps` is set. A
local role (no dot) is skipped. A collection's role (two or more dots) is
skipped with a warning. One run resolves at most 1000 roles, dependencies
included, and exits `2` past that.

Roles are taken level by level, in the order the file lists them, and the
first role to take an install name keeps it: a later one with that name is
skipped, with a warning when it asks for another source or version, as in
ansible-galaxy.

### An existing role directory

| The directory under `roles_path` holds | go-galaxy |
| --- | --- |
| go-galaxy's `.extract-done` marker | Kept when it holds the same source and version, else replaced |
| `meta/.galaxy_install_info` only, from `ansible-galaxy` | Replaced, with a warning |
| Neither | Refused, exit `5`: remove the directory to let go-galaxy manage it |

## galaxy.toml

`galaxy.toml` holds the two lists in a `[project]` table, and run settings in
[`[tool.go-galaxy]`](../reference/configuration.md#the-toolgo-galaxy-table).

### The `[project]` table

```toml
[project]
name = "infra"
collections = [
  "community.general >= 10.0, < 12.0", # (1)!
  "git+https://github.com/acme/mono.git#collections/app,main", # (2)!
  "https://dl.example.com/acme-lib-2.1.0.tar.gz",
  { name = "acme.app", version = ">= 1.4.0", source = "hub" }, # (3)!
]
roles = ["geerlingguy.docker,8.0.0"] # (4)!
```

1.  The name, then a [constraint](#version-constraints).
2.  A git source, never split into a constraint.
3.  An [inline table](#inline-tables-and-projectcollections).
4.  `src,version,name`, never split into a constraint.

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `name`, `version`, `description` | String | No | For readers; go-galaxy ignores them |
| `collections` | Array of strings and tables | This or `roles` | [Collections](#collections) |
| `roles` | Array of strings and tables | This or `collections` | [Roles](#roles) |

`collections = []` installs nothing. A file with neither list exits `2`. A
`${VAR}` in an entry stays literal: only
[`[tool.go-galaxy]`](../reference/configuration.md#var-expansion) expands one.

### Collection strings

```mermaid
flowchart LR
  S{"String starts with"} -->|git+ or git@| GS["git source"]
  S -->|http or https| US["url source"]
  S -->|path or other scheme| R["Refused, exit 2"]
  S -->|a name| N{"After the name"}
  N -->|nothing| A["Galaxy, any version"]
  N -->|valid constraint| OK["Galaxy, that constraint"]
  N -->|anything else| R
```

The name is the longest run of `A-Za-z0-9_.`, followed by whitespace or an
operator, so `ns.name>=1.0` equals `ns.name >= 1.0`.

<details markdown>
<summary>Refused spellings</summary>

A name running straight into its constraint and an unparsable constraint stop
at load:

```text
✗ failed to load requirements file: collections[0]: invalid collection name: "ns.name@1.0": put a space or a version operator between the name and its constraint
✗ failed to load requirements file: collections[0]: invalid collection name: "ns.name:1.0": put a space or a version operator between the name and its constraint
✗ failed to load requirements file: collections[0]: invalid collection name: "ns.name-1.0": put a space or a version operator between the name and its constraint
✗ failed to load requirements file: collections[0]: invalid collection name: "ns.name1.0.0"
✗ failed to load requirements file: collections[0]: invalid collection version constraint: ">>= 1.0" for ns.name
```

`ns.name1.0.0` is a four-part name, since nothing separates the version. Only
the grammar is checked at load: `ns.name >= 99` loads and fails in the
resolve.

</details>

### Inline tables and `[[project.collections]]`

```toml title="Inline tables"
[project]
collections = [{ name = "acme.app", version = ">= 1.4.0", source = "hub" }]
roles = [{ name = "postgres", src = "geerlingguy.postgresql", version = "3.5.0" }]
```

```toml title="Array of tables"
[[project.collections]]
name = "acme.app"
version = ">= 1.4.0"
source = "hub"

[[project.roles]]
name = "postgres"
src = "geerlingguy.postgresql"
version = "3.5.0"
```

A collection table takes the keys in the [Collections](#collections) table. A
role table takes `name`, `src`, `scm` and `version`, plus `role`: ansible's
older spelling of `name`, which also serves as `src` when `src` is absent.
Every value is a string, except that `signatures` may be an array of strings.

### Stricter than requirements.yml

| Case | requirements.yml | galaxy.toml |
| --- | --- | --- |
| Unknown key on a collection | Ignored | Refused, exit `2` |
| Unknown key on a role | Dropped with a warning | Refused, exit `2` |
| Other top-level key or table | Ignored | Refused, exit `2` |
| A number, `true` or a date where text belongs | Read as the text written | Refused, exit `2` |
| Unparsable constraint | Exit `1` at the resolve | Exit `2` at load |

## Which file is read

```mermaid
flowchart LR
  A{"-r or a variable set?"} -->|yes| B{"Path ends in?"}
  B -->|".toml"| T["Read as galaxy.toml"]
  B -->|".yml or .yaml"| Y["Read as requirements.yml"]
  B -->|"anything else"| X["Exit 2"]
  A -->|no| C{"./galaxy.toml a regular file?"}
  C -->|yes| T
  C -->|no| D["Read ./requirements.yml"]
```

The flag is `-r`, also spelled `--requirements-file` or ansible's
`--role-file` ([Paths and files](../reference/cli.md#paths-and-files)). Its
variables are `GO_GALAXY_REQUIREMENTS_FILE` and
`ANSIBLE_GALAXY_REQUIREMENTS_FILE`. The value must end in `.yml`, `.yaml` or
`.toml`, in any case, so `.TOML` reads as `galaxy.toml`. Any other value exits
`2` before a file is read, whatever the command: a `galaxy.txt`, a name with no
extension, `/dev/stdin` and `<(...)`. So does an empty export, which counts as
set and names no file.

A requirements file that exists but is not a regular file, such as a
directory or a named pipe, exits `2` before it is opened wherever it is
read: from `install`, `warm`, `lock` and `tree`, from `hash` with no
`galaxy.lock` beside it, and from every command given such a `galaxy.toml`,
whose settings each one reads. `outdated` never reads a `requirements.yml`,
and `cleanup` never reads the one `-r` names: it reloads, through the same
check, the files its project records name
([What cleanup keeps](caching.md#what-cleanup-keeps)). `explain` takes no
roots from a `requirements.yml` it cannot load
([explain](../internals/flow-hash-tree-explain.md#explain)). A symlink to a
regular file is read as that file.

When discovery finds both files, go-galaxy warns:

```text
! galaxy.toml and requirements.yml are both present in the current directory; using galaxy.toml and ignoring requirements.yml (name one with --requirements-file to choose)
```

With neither file, `install`, `warm` and `lock` exit `2`. So does `hash`,
unless a `galaxy.lock` is present ([A cache key for CI](lockfile.md#a-cache-key-for-ci)).

Unlike `./ansible.cfg`
([Where it is found](../reference/configuration.md#where-it-is-found)), a
requirements file is still discovered in a world-writable directory. In a
shared directory such as `/tmp`, name the file with `-r`
([Trust model](security.md#trust-model)).

## What is refused

Each exits `2` before anything installs, naming the entry.

| Entry | Why | Write instead |
| --- | --- | --- |
| A `file`, `dir` or `subdirs` type, a local path, or another scheme such as `ssh://` without `git+` | Unsupported source | A Galaxy name, `git+ssh://...` or an https tarball |
| A credential in a URL | It would reach logs and the lockfile | A binding for [git](servers-and-auth.md#git-sources-and-credentials) or [url](servers-and-auth.md#url-sources-and-credentials) |
| A `#fragment` in a url source | Not part of a tarball URL. A query is allowed. Progress and error lines leave it out, but `galaxy.lock` records it, so keep tokens out of it | The URL without it |
| `src:` or `scm:` in a collection | Role keys | The entry under `roles:` |
| A collection named twice, even by two repositories | One entry per collection | One entry |
| `signatures:` on a git or url collection | Signatures cover Galaxy collections only | Drop it: the pin covers it |
| `include:` in `roles:` | No second file is read | The roles, inline |
| An `scm:` other than `git` | No external process runs | A git repository |
| A role URL neither git nor `.tar.gz`, or a local path | Not an installable role source | `git+<url>` or a `.tar.gz` URL |
| `#subdir` on a git role | The repository root is the role | One repository per role |
| `source:`, `signatures:` or `type:` on a role | It would change the entry's meaning | The entry without it |
| Two roles with one install name, case ignored | Both would land in one directory | Distinct `name:` values |
| A key an entry is read by, written with no value, such as `version:`, `name: ~` or `source: null`, or a list item that is only `-` | Nothing says what was meant: ansible reads some as absent and fails on others | The value, or the entry without the key |
| A list or a mapping under a key that takes text, such as `type: [git]` | The key holds one string; only `signatures:` takes a list | That string |

## Moving to galaxy.toml

- [ ] Upgrade every go-galaxy sharing the cache or the lockfile first
      ([From v1.2.x](../reference/upgrading.md#from-v12x)).
- [ ] Rewrite the entries in `galaxy.toml`. A constraint may be respelled as
      [Version constraints](#version-constraints) allows, such as `"1.0.0"`
      as `== 1.0.0`, and still replays the last resolution.
- [ ] Delete `requirements.yml`, or every run that names no file warns.
- [ ] With a lockfile, run `go-galaxy lock --check`: the same entries give
      the same `galaxy.lock`. Without one,
      [`go-galaxy hash`](lockfile.md#a-cache-key-for-ci) keeps its key.
- [ ] Optionally move `cache_dir` and the whole server list from `ansible.cfg`
      into [`[tool.go-galaxy]`](../reference/configuration.md#the-toolgo-galaxy-table).

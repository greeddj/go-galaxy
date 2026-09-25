# Compatibility with ansible-galaxy

go-galaxy reads your files and variables as `ansible-galaxy` does, apart from
the differences below, measured against ansible-core 2.21.2.

| Surface | go-galaxy |
| --- | --- |
| `requirements.yml`, `collections:` and `roles:` | Read the same, except as below |
| `ansible.cfg` paths, servers, `server_timeout` and `cache_dir`, their `ANSIBLE_*` variables, and `ANSIBLE_GALAXY_*` signature variables | Read the same, except as below ([every key](configuration.md#what-go-galaxy-reads)) |
| `ansible.cfg` `[galaxy]` signature keys | Named in a warning, never read |
| `~/.ansible/galaxy_token`, `[galaxy] token_path` | Not read |
| `galaxy.toml` | go-galaxy only |

## Differences a migration runs into

> [!WARNING]
> These most often break a pipeline that worked under `ansible-galaxy`:
>
> - A [failing server](#servers-and-tokens) stops the run; only a `404`
>   moves on
> - Your token is [refused](#servers-and-tokens) beside a file-chosen server
>   URL or relaxed TLS
> - `~/.ansible/galaxy_token` is not read, so a private hub fails with
>   [`4`](exit-codes.md) ([pass a token](servers-and-auth.md#--token))
> - [Exit codes](#exit-codes-are-not-ansibles) mean other things
> - [Installed files](#installed-files-are-read-only) are read-only
> - [Unsupported sources](#sources) and [flags](#command-and-flag-cheat-sheet)
>   exit `2` before any request

### Command and flag cheat sheet

| ansible-galaxy | go-galaxy |
| --- | --- |
| `collection install -r f`, `role install --role-file f` | `go-galaxy install -r f` (`--role-file` works too); bare `go-galaxy` finds the file |
| `collection install ns.name` | Add it to the requirements file; positional names exit `2` |
| `-p`, `--collections-path` | `-p`, `--download-path` |
| `role install -p dir` | `--roles-path dir`: `-p` is the collections path |
| `-s`, `--api-key`, `-n` | `--server`, `--token`, `--no-deps` |
| `-U`, `--force`, `--force-with-deps` | None: the resolve decides; `--refresh` asks the servers again |
| `--pre` | None: an exact pin or a `>=X.Y.Z-0` floor ([Prereleases](#prereleases)) |
| `-c`, `--ignore-certs`, `[galaxy] ignore_certs`, `ANSIBLE_GALAXY_IGNORE` | None: trust the CA with `SSL_CERT_FILE`, or set a server's [`validate_certs`](servers-and-auth.md#tls-validate_certs) |
| `--ignore-signature-status-codes A B` | `--ignore-signature-status-code`, once per code |
| `-i`, `--ignore-errors`, `--clear-response-cache` | None; each exits `2` |
| `role list`, `remove`, `init`, `search` | None; [`cleanup`](cli.md#cleanup-options) removes roles no project reaches |

### Exit codes are not ansible's

| Code | ansible-galaxy | go-galaxy |
| --- | --- | --- |
| `1` | Generic error | Generic failure |
| `2` | Usage error | Usage or configuration error |
| `4` | Parser error | Network or Galaxy API failure |
| `5` | Options error | Install-time failure |
| `99` | Interrupted | Not used |
| `250` | Unexpected error | Not used |
| `129`, `130`, `143` | Not used | [Interrupted](exit-codes.md#signals) by SIGHUP, SIGINT or SIGTERM |

Codes `3` and `6` to `10` are in [Exit codes](exit-codes.md).

## Deliberate differences

### Servers and tokens

| Area | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| Several servers | Merges versions from all of them | The first server in [`server_list`](servers-and-auth.md#how-a-collection-picks-its-server) that has the collection owns it | Order `server_list`, or pin with `source:` |
| A server fails | Skips it | Any error but `404` stops the run: exit `4`, or `1` if the host is unreachable | Fix the token or the server |
| Your token with a file-chosen URL or relaxed TLS | Sends it | Refused, exit `2` | Follow [`--token`](servers-and-auth.md#--token) |
| `[galaxy_server.<id>] timeout` | Read | Ignored with a warning | Use `--timeout` or `server_timeout` |
| `username`, `password`, `auth_url`, `client_id` | Basic or Keycloak login | Refused, exit `2` | Use an API token |

### Versions and resolution

| Area | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| No version fits | Lists the unmet requirements, exit `1` | Prints PubGrub's proof, exit `3` | [Read the proof](requirements.md#when-no-version-fits) |
| Already installed | Kept unless `-U` or `--force` | Never picks the version; a lower result installs over it | Pin with [`lock`](lockfile.md#create-the-lockfile) |
| Constraint grammar | Comparison operators, comma-joined; `1.0` means `1.0.0` | Adds `1.x`, `~1.2`, `^1.2`, `1.2 - 1.4`, <code>&#124;&#124;</code>; `1.0` means `1.0.x` | Use [ansible's operators](requirements.md#version-constraints) and full `X.Y.Z` in a shared file |
| `requires_ansible` | Skips versions that exclude the running core | Not read | Pin a version your ansible-core supports |

A rerun replays the last resolution until the requirements, the servers or
`--no-deps` change, or you pass `--refresh` or `--no-cache`
([What a rerun reuses](caching.md#what-a-rerun-reuses)).

### Prereleases

| Constraint | ansible-galaxy | go-galaxy |
| --- | --- | --- |
| Plain, only prereleases published | Installs a prerelease | Fails, hinting at an exact pin or a `-0` floor |
| `>=1.0.0-0` | Releases only, unless `--pre` | Prereleases too, `2.0.0-rc1` included |
| None, empty or `*` | Newest release, unless `--pre` | Newest version, prereleases included |
| Exact prerelease pin | Installs it | Installs it |

A `>=1.0.0` floor keeps a resolve on releases.

### Sources

| Source | Collections | Roles | [Pinned](lockfile.md#what-each-entry-is-pinned-by) by |
| --- | --- | --- | --- |
| Galaxy server | Yes | Yes, through the v1 role API | Version and sha256; a role by commit |
| `git+` or `git@` repository | Yes | Yes | Commit |
| http(s) tarball | Yes | `.tar.gz` only | sha256 of the bytes |
| `file`, `dir`, `subdirs`, local path | Refused, exit `2` | Refused, exit `2` | - |
| `scm: hg` | - | Refused, exit `2` | - |
| A bare top-level list | Read as collections | Not read: put roles under `roles:` | - |

Refused rows fail at load ([What is refused](requirements.md#what-is-refused)).
A url collection's `version:` must match its `MANIFEST.json`, or the run
exits `3`.

No `git` binary runs, so credential helpers, `~/.netrc` and `~/.ssh/config`
are not read, and an ssh URL must name its user (`ssh://git@host/...`). Set
credentials and `known_hosts` per
[Git sources and credentials](servers-and-auth.md#git-sources-and-credentials).

<details markdown>
<summary>Git build details</summary>

| Spelling | Means |
| --- | --- |
| `<url>#sub,main` | Subdirectory `sub`, ref `main` |
| `<url>,main#sub` | Ref `main#sub`, which fails at the remote |

- `version:` is a branch, a tag, `refs/heads/...`, `refs/tags/...` or a full
  40-digit commit, `HEAD` by default; an abbreviated commit is refused.
- A name that is both a branch and a tag means the branch, with a warning;
  write `refs/tags/<name>` for the tag.
- `galaxy.yml` needs an exact `MAJOR.MINOR.PATCH` version, where ansible
  installs a missing one as `*`.
- A directory holding both `galaxy.yml` and `MANIFEST.json` is refused, exit
  `2`; ansible uses the `MANIFEST.json`.
- `dependencies:` keys must be `namespace.name`. A git URL or path key, which
  ansible accepts, exits `3`.
- Two directories declaring the same `namespace.name` are refused, exit `2`.
- `build_ignore` is honored; `manifest:` directives are refused.
- Submodules are skipped with a warning.

How a repository becomes an artifact is in
[Git discovery](architecture.md#git-discovery); what is validated is in
[Git and url sources](security.md#git-and-url-sources).

</details>

### Paths and timeouts

| Setting | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| `collections_path`, `roles_path` | Searches every `:` entry | First entry only, the rest warned about | List one path |
| Default paths | `~/.ansible/collections`, `~/.ansible/roles` | `.collections`, `.roles` in the working directory | Point ansible's paths there |
| `~` and `$VAR` | Expanded in every path | Keyring path only: a leading `~` or `~/` | Write full paths |
| [`--timeout`](cli.md#timeouts-and-fixed-limits) | `60`, whole seconds only | `30s`; also a duration such as `90s` | Raise it for a slow hub; whole seconds in shared files |

### Installed files are read-only

```text
.collections/ansible_collections/
  community/general/
    README.md                     -r--r--r--
  community.general-13.4.0.info/
    GALAXY.yml                    -rw-r--r--
    .extract-done.<sha256>        -rw-r--r--
.roles/geerlingguy.docker/
  meta/main.yml                   -r--r--r--
  meta/.galaxy_install_info       -rw-r--r--
  .extract-done.<sha256>          -rw-r--r--
```

Extracted files have no write bit, since an install may
[share their bytes](architecture.md#extracted-store-content-addressed-materialized-by-hardlink)
with the cache and other installs; directories and sidecar files stay
writable. An in-place edit that worked after `ansible-galaxy` fails; edit
a copy outside the tree.

The `.extract-done` marker counts entries and bytes, so an edit that changes
a file's size is extracted over on the next install. A git or url collection
also gets `go-galaxy.yml` in `.info`, holding its commit or sha256; ansible
ignores it.

### Signatures

| Area | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| Verifier | Runs `gpg` | Pure Go; a `.kbx` keybox is refused, exit `2` | Export with `gpg --export --armor` |
| Required count | Counts signatures | Counts distinct signing keys | Sign with distinct keys |
| `FILES.json` | Checks each listed file's hash | [Checked both ways](signatures.md#manifest-chain-check) once a signature verifies | - |

### galaxy.toml

| Files present | go-galaxy reads, with no `-r` |
| --- | --- |
| `requirements.yml` | `requirements.yml` |
| `galaxy.toml` | `galaxy.toml` |
| Both | `galaxy.toml`, warning that `requirements.yml` is ignored |

`ansible-galaxy` reads only what `-r` names, and never `galaxy.toml`. A
server list in [`[tool.go-galaxy]`](configuration.md#the-toolgo-galaxy-table)
replaces `ansible.cfg`'s `server_list` and `[galaxy_server.*]` for go-galaxy
only ([galaxy.toml](requirements.md#galaxytoml)).

## Roles

Roles install under `roles_path` with ansible's `meta/.galaxy_install_info`.
A Galaxy role is fetched by git at its tag, not as a GitHub tarball
([Roles and the v1 role API](servers-and-auth.md#roles-and-the-v1-role-api)).

| An existing role directory holds | go-galaxy |
| --- | --- |
| Its `.extract-done` marker | Replaced when the source changed |
| `meta/.galaxy_install_info` only | Replaced, with a warning |
| Neither | Refused, exit `5` |

`ansible-galaxy` skips an installed role unless `--force`.

| Galaxy role version | ansible-galaxy | go-galaxy |
| --- | --- | --- |
| None asked | Highest tag by `LooseVersion`, ties in server order | Same, ties to the greater tag name |
| No tags listed | `github_branch`, else `master`; an asked version is tried as a ref | Same |
| Tags do not compare | Fails, asking for an explicit version, exit `1` | Same, exit `3` |
| Asked, not among listed tags | Only `master` passes | `github_branch` or `master` passes, else exit `3` |

<details markdown>
<summary>Role edge cases</summary>

- A Galaxy name has exactly one dot, `owner.role`; ansible splits at the
  last dot.
- A three-part `ns.collection.role` dependency is skipped with a warning, as
  ansible skips it.
- Without `scm: git`, an http(s) `src:` is a git role only when its host is
  exactly `github.com`; ansible matches the text anywhere.
- An http(s) `src:` ending in `.tar.gz` is a url role.
- `meta/main.yml`, else `meta/main.yaml`, at the repository root marks the
  role; a tree with both is refused.
- Files marked `export-ignore` in `.gitattributes` are installed; ansible's
  GitHub tarball leaves them out.
- A dependency's `version: 1.10` is the text `1.10`; ansible reads the
  float `1.1`.
- The first request for an install name wins; a later different one is
  warned about, where ansible is silent.
- One run finds at most 1000 roles.

</details>

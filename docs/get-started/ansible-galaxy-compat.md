# Coming from ansible-galaxy

go-galaxy reads your files and variables as `ansible-galaxy` does, apart from
the differences below, measured against ansible-core 2.21.2 (the `~`, `$VAR`,
`{{CWD}}` and `ANSIBLE_CONFIG` rows, the `-r` file name, an unquoted version and
a key written with no value against 2.21.3).

| Surface | go-galaxy |
| --- | --- |
| `requirements.yml`, `collections:` and `roles:` | Read the same, except as below |
| `ansible.cfg` paths, servers, `server_timeout` and `cache_dir`, their `ANSIBLE_*` variables, and `ANSIBLE_GALAXY_*` signature variables | Read the same, except as below ([What go-galaxy reads](../reference/configuration.md#what-go-galaxy-reads)) |
| `ansible.cfg` `[galaxy]` signature keys | Named in a warning, never read |
| `~/.ansible/galaxy_token`, `[galaxy] token_path` | Not read |
| `galaxy.toml` | go-galaxy only |

## Differences a migration runs into

> [!WARNING]
> These most often break a pipeline that worked under `ansible-galaxy`:
>
> - With no path configured, collections and roles install into
>   [`.collections` and `.roles`](#paths-and-timeouts), where
>   `ansible-playbook` does not look
>   ([Point ansible at the installs](getting-started.md#point-ansible-at-the-installs))
> - A [failing server](#servers-and-tokens) stops the run. Only a `404` moves
>   on to the next server
> - A token you export or pass is [refused](#servers-and-tokens), exit `2`,
>   when its server's `url` or `validate_certs = false` is set only in
>   `ansible.cfg` or `galaxy.toml`: export that setting too
> - A server section with `username`, `password`, `auth_url` or `client_id`
>   [exits `2`](#servers-and-tokens): use an API token
> - `~/.ansible/galaxy_token` is not read, so a private hub fails with
>   [`4`](../reference/exit-codes.md): pass the token as
>   [A private hub, then public Galaxy](../guides/servers-and-auth.md#a-private-hub-then-public-galaxy)
>   shows
> - [Exit codes](#exit-codes-are-not-ansibles) mean other things
> - [Installed files](#installed-files-are-read-only) are read-only
> - [Unsupported sources](#sources) and [flags](#command-and-flag-cheat-sheet)
>   exit `2` before any request
> - A requirements file not named `*.yml`, `*.yaml` or `*.toml`, such as
>   `/dev/stdin`, [exits `2`](#command-and-flag-cheat-sheet)
> - An entry key [written with no value](#versions-and-resolution), such as a
>   bare `version:`, exits `2`, where ansible reads it as absent on a git
>   collection or a role

### Command and flag cheat sheet

An `ansible-galaxy` spelling missing from the go-galaxy column, such as `-s`
or `--force`, exits `2` as an unknown flag.

| ansible-galaxy | go-galaxy |
| --- | --- |
| `collection install -r f`, `role install --role-file f` | `go-galaxy install -r f`, or `--role-file f`. Bare `go-galaxy` finds the file. A roles-only list needs its entries under `roles:`. `f` must end in `.yml`, `.yaml` or `.toml`, any case, else exit `2`, where `collection install` reads any name, `/dev/stdin` included, and `install` and `role install` only a lower-case `.yml` or `.yaml` ([Which file is read](../guides/requirements.md#which-file-is-read)) |
| `collection install ns.name` | Add it to the requirements file. A positional name exits `2` |
| `-p`, `--collections-path` | `-p`, `--download-path` |
| `role install -p dir` | `--roles-path dir`: `-p` is the collections path |
| `-s`, `--api-key`, `-n` | `--server`, `--token`, `--no-deps` |
| `-U`, `--force`, `--force-with-deps` | None: the resolve decides, and `--refresh` asks the servers again |
| `--pre` | None: an exact pin or a `>=X.Y.Z-0` floor ([Prereleases](#prereleases)) |
| `-c`, `--ignore-certs` | None: trust the CA with `SSL_CERT_FILE`, or set a server's `validate_certs` ([TLS and a private CA](../guides/servers-and-auth.md#tls-and-a-private-ca)) |
| `[galaxy] ignore_certs`, `ANSIBLE_GALAXY_IGNORE` | Ignored without a warning: trust the CA as above |
| `--ignore-signature-status-codes A B` | `--ignore-signature-status-code`, once per code |
| `-i`, `--ignore-errors`, `--clear-response-cache` | None |
| `role list`, `remove`, `init`, `search` | None. `cleanup` removes roles no project reaches ([What cleanup keeps](../guides/caching.md#what-cleanup-keeps)) |

### Exit codes are not ansible's

| Code | ansible-galaxy | go-galaxy |
| --- | --- | --- |
| `1` | Generic error | Generic failure |
| `2` | Usage error | Usage or configuration error |
| `4` | Parser error | Network or Galaxy API failure |
| `5` | Options error | Install-time failure |
| `99` | Interrupted | Not used |
| `250` | Unexpected error | Not used |
| `129`, `130`, `143` | Not used | [Interrupted](../reference/exit-codes.md#signals) by SIGHUP, SIGINT or SIGTERM |

`ansible-galaxy` exits `1` for most failures. go-galaxy splits them over codes
`2` to `10` ([Exit codes](../reference/exit-codes.md)), so a script should
test for non-zero or branch on the code.

## Differences by area

### Servers and tokens

| Area | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| Several servers | Merges versions from all of them | The first server in [`server_list`](../guides/servers-and-auth.md#how-a-collection-picks-its-server) that has the collection owns it | Order `server_list`, or bind the collection with `source:` |
| A server fails | Skips it | Any error but `404` stops the run: exit `4`, or `5` at install ([When several things fail](../reference/exit-codes.md#when-several-things-fail)) | Fix the token or the server |
| A token you export or pass, with the server's `url` or `validate_certs = false` set in `ansible.cfg` | Sends it | Refused, exit `2` | Export the same value as `ANSIBLE_GALAXY_SERVER_<ID>_URL` or `_VALIDATE_CERTS` ([Where a token may go](../guides/servers-and-auth.md#where-a-token-may-go)) |
| `[galaxy_server.<id>] timeout` | Read | Ignored with a warning | Use `--timeout` or `server_timeout` |
| `username`, `password`, `auth_url`, `client_id` | Basic or Keycloak login | Refused, exit `2` | Use an API token |

### Versions and resolution

| Area | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| No version fits | Lists the unmet requirements, exit `1` | Prints PubGrub's proof, exit `3` | [Read the proof](../guides/requirements.md#when-no-version-fits) |
| Already installed | Kept unless `-U` or `--force` | Ignored by the resolve, so a lower result installs over it | Pin with [`lock`](../guides/lockfile.md#create-the-lockfile) |
| Rerun | Asks the servers again, keeping an installed version that fits | Reuses the last resolution, even into an empty tree, until the requirements, the servers or `--no-deps` change | Pass `--refresh` for new releases ([What a rerun reuses](../guides/caching.md#what-a-rerun-reuses)) |
| Constraint grammar | Comparison operators, comma-joined; `1.0` means `1.0.0` | Adds `1.x`, `~1.2`, `^1.2`, `1.2 - 1.4`, <code>&#124;&#124;</code>; `1.0` means `1.0.x` | Use [ansible's operators](../guides/requirements.md#version-constraints) and full `X.Y.Z` in a shared file |
| `requires_ansible` | Skips versions that exclude the running core | Not read | Pin a version your ansible-core supports |
| An unquoted version, such as `1.10` | Read as a YAML number and written back, so `1.10` becomes `1.1`: a git collection checks out `1.1`, a Galaxy role asks for `1.1`, and a Galaxy collection or a git role fails with exit `250` | The text written, `1.10`, in the requirements file and in a role's `meta/main.yml` and `meta/requirements.yml` | [Quote versions](../guides/requirements.md#version-constraints) in a file both tools read |
| A key written with no value, such as `version:` | A Galaxy collection fails with exit `250`; a git collection or a role reads it as absent | Refused, exit `2` ([What is refused](../guides/requirements.md#what-is-refused)). In a role's `meta` files it reads as absent, as in ansible | Write the value, or leave the key out |

### Prereleases

| Constraint | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| None, empty or `*` | Newest release, unless `--pre` | Newest version, prereleases included | Add a release floor, such as `>=1.0.0`, to stay on releases |
| Exact prerelease pin | Installs it | Installs it | - |
| `>=1.0.0-0` | Releases only, unless `--pre` | Prereleases too, `2.0.0-rc1` included | - |
| A constraint naming no prerelease, with only prereleases published | Installs a prerelease | Fails with exit `3`, hinting at an exact pin or a `-0` floor | - |

### Sources

| Source | Collections | Roles |
| --- | --- | --- |
| Galaxy server | Yes | Yes, through the v1 role API |
| `git+` or `git@` repository | Yes | Yes |
| http(s) tarball | Yes | `.tar.gz` only |
| `file`, `dir`, `subdirs`, local path | Refused, exit `2` | Refused, exit `2` |
| `scm: hg` | - | Refused, exit `2` |
| A bare top-level list, ansible's roles-only format | Read as collections | Not read: move the entries under `roles:` |

Refused rows fail at load ([What is refused](../guides/requirements.md#what-is-refused)).
What `galaxy.lock` records for each source is in
[What each entry is pinned by](../guides/lockfile.md#what-each-entry-is-pinned-by).
A url collection's `version:` must match its `MANIFEST.json`, or the run
exits `3`.

No `git` binary runs, so credential helpers, `~/.netrc` and `~/.ssh/config`
are not read, and an ssh URL must name its user (`ssh://git@host/...`). Set
credentials and `known_hosts` per
[Git sources and credentials](../guides/servers-and-auth.md#git-sources-and-credentials).

A ref that names both a branch and a tag means the branch, with a warning,
for a collection and a role alike. ansible builds a collection from a clone
checked out at the ref, so it takes the tag unless the name is the
repository's default branch. For a role it then runs `git archive` at the
ref, which always takes the tag, unless `--keep-scm-meta` packs the checkout
instead. Spell `refs/tags/<name>` for the tag
([Collections](../guides/requirements.md#collections)).

<details markdown>
<summary>Git collections ansible accepts and go-galaxy refuses</summary>

- An abbreviated commit as the ref
- A `galaxy.yml` without an exact `MAJOR.MINOR.PATCH` version, where ansible
  installs a missing one as `*`
- A directory holding both `galaxy.yml` and `MANIFEST.json`, where ansible
  uses the `MANIFEST.json`
- A `dependencies:` key that is a git URL or a path, which exits `3`
- A `manifest:` key in `galaxy.yml`

The rules are in
[What a git repository must hold](../guides/requirements.md#what-a-git-repository-must-hold).

</details>

### Paths and timeouts

| Setting | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| `collections_path`, `roles_path` | Searches every `:` entry | First entry only, the rest warned about | List one path |
| Default paths | `~/.ansible/collections`, `~/.ansible/roles` | `.collections`, `.roles` in the working directory | [Point ansible at the installs](getting-started.md#point-ansible-at-the-installs) |
| `~` and `$VAR` in `-r`, `-p`, `--roles-path` or the keyring path (`--keyring`, `ANSIBLE_GALAXY_GPG_KEYRING`) | Expanded | Taken as written, as are these flags' `GO_GALAXY_*` variables and `ANSIBLE_GALAXY_GPG_KEYRING`, except a bare `~` or a leading `~/` in the keyring path. A `galaxy.toml` expands [`${VAR}`](../reference/configuration.md#var-expansion) in `[tool.go-galaxy]` | Write full paths |
| A `$VAR` whose value holds `:` in `ANSIBLE_COLLECTIONS_PATH` or `ANSIBLE_ROLES_PATH` | One path, the `:` kept | Split at that `:`, the first part used and the rest warned about | Name one path |
| `~user`, or `~` with `HOME` unset, in an `ansible.cfg` path or its `ANSIBLE_*` variable, on Linux | The home the system user database lists (`getpwnam` for `~user`, `getpwuid` for `~`) | The home `/etc/passwd` lists. For a user only a directory service such as LDAP knows, the `~` stays as written | Write the home directory out |
| `{{CWD}}` in an `ansible.cfg` path or its `ANSIBLE_*` variable | The working directory | Kept as written: a directory named `{{CWD}}` ([ansible.cfg paths](../reference/configuration.md#ansiblecfg-paths)) | Write the path relative to the file, or absolute |
| An `ANSIBLE_CONFIG` empty before or after expansion | Reads `./ansible.cfg`, even in a world-writable directory | Skipped, and a world-writable directory still skips `./ansible.cfg` | Unset it, or name the file |
| [`--timeout`](../reference/cli.md#timeouts-and-fixed-limits) | `60`, whole seconds only | `30s`, whole seconds or a duration such as `90s` | Raise it for a slow hub. Use whole seconds in a shared file |

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

Extracted files have no write bit, since an install may share their bytes
with the cache and other installs
([The local cache](../guides/caching.md#the-local-cache)). Directories and
sidecar files stay writable. An in-place edit that worked after
`ansible-galaxy` fails, so edit a copy outside the tree.

The `.extract-done` marker records how many entries and bytes the install
holds. The next install extracts over an edit that changes those totals and
keeps one that does not
([What a frozen install checks](../guides/lockfile.md#what-a-frozen-install-checks)).
A git or url collection also gets a `go-galaxy.yml` in its `.info`
directory, holding its commit or sha256. ansible ignores it.

### Signatures

| Area | ansible-galaxy | go-galaxy | What you do |
| --- | --- | --- | --- |
| Verifier | Runs `gpg` | Pure Go, which refuses a `.kbx` keybox, exit `2` | Export the keybox's keys as [Keyring and signature file formats](../guides/signatures.md#keyring-and-signature-file-formats) shows: a bare `gpg --export --armor KEYID` reads the default keyring and misses a dedicated keybox |
| Required count | Counts signatures | Counts distinct signing keys | Sign with distinct keys |
| `FILES.json` | Checks each listed file's hash | Also refuses an archive entry it does not list, exit `7`, once a signature verifies ([Manifest chain check](../guides/signatures.md#manifest-chain-check)) | - |

### galaxy.toml

With no `-r`, go-galaxy reads `./galaxy.toml` before `./requirements.yml` and
warns when both exist
([Which file is read](../guides/requirements.md#which-file-is-read)).
`ansible-galaxy` reads only what `-r` names, and never `galaxy.toml`. A
server list in [`[tool.go-galaxy]`](../reference/configuration.md#the-toolgo-galaxy-table)
replaces `ansible.cfg`'s `server_list` and `[galaxy_server.*]` for go-galaxy
only.

### Roles

Roles install under `roles_path` with ansible's `meta/.galaxy_install_info`.
A Galaxy role is fetched by git at its tag, not as a GitHub tarball
([Roles and the v1 role API](../guides/servers-and-auth.md#roles-and-the-v1-role-api)).

`ansible-galaxy` skips an installed role unless `--force`. go-galaxy replaces
a role `ansible-galaxy` installed, with a warning, and refuses a directory
neither tool installed
([An existing role directory](../guides/requirements.md#an-existing-role-directory)).

| Galaxy role version | ansible-galaxy | go-galaxy |
| --- | --- | --- |
| None asked | Highest tag by `LooseVersion`, ties in server order | Same, ties to the greater tag name |
| No tags listed | `github_branch`, else `master`; an asked version is tried as a ref, tag or branch | `github_branch`, else `master`; an asked version is fetched as a branch only, so a tag the server does not list exits `3` |
| Tags do not compare | Fails, asking for an explicit version, exit `1` | Same, exit `3` |
| Asked, not among listed tags | Only `master` passes | `github_branch` or `master` passes, else exit `3` |

<details markdown>
<summary>Role edge cases</summary>

- A Galaxy name has exactly one dot, `owner.role`, where ansible splits at
  the last dot.
- Without `scm: git`, an http(s) `src:` is a git role only when its host is
  exactly `github.com`, where ansible matches the text anywhere.
- An abbreviated commit as a git role's ref exits `2`, where ansible checks it
  out. Spell the full 40-hex commit.
- A role carrying both `meta/main.yml` and `meta/main.yaml` is refused, where
  ansible reads `meta/main.yml`.
- Files marked `export-ignore` in `.gitattributes` are installed, where
  ansible, which installs from an archive, leaves them out.
- A dependency whose install name is already requested is skipped, with a
  warning when it asks for another source or version. ansible skips it
  silently. Two entries of the requirements file with one install name are
  refused ([What is refused](../guides/requirements.md#what-is-refused)).
- One run resolves at most 1000 roles
  ([Roles](../guides/requirements.md#roles)).

</details>

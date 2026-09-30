# Servers and credentials

Install from private Galaxy servers, git repositories and tarball URLs, and
see where each credential may go.

| I need to...                                       | Go to                                                                  |
|:---------------------------------------------------|:-----------------------------------------------------------------------|
| Use a private hub with a token, then public Galaxy | [A private hub, then public Galaxy](#a-private-hub-then-public-galaxy) |
| Pass a token, or fix one that is refused           | [Where a token may go](#where-a-token-may-go)                          |
| Trust a hub signed by a private CA                 | [TLS and a private CA](#tls-and-a-private-ca)                          |
| Install Galaxy roles                               | [Roles and the v1 role API](#roles-and-the-v1-role-api)                |
| Fetch a private git repository                     | [Git sources and credentials](#git-sources-and-credentials)            |
| Download a private release tarball                 | [URL sources and credentials](#url-sources-and-credentials)            |

## A private hub, then public Galaxy

=== "galaxy.toml"

    ```toml
    [project]
    collections = ["community.general"]

    [[tool.go-galaxy.servers]]
    id = "hub"
    url = "https://hub.example.internal/api/galaxy"
    token = "${HUB_TOKEN}" # (1)!

    [[tool.go-galaxy.servers]]
    id = "galaxy"
    url = "https://galaxy.ansible.com"
    ```

    1.  Expanded when the file loads. An unset `HUB_TOKEN` exits
        [`2`](../reference/exit-codes.md)
        ([`${VAR}` expansion](../reference/configuration.md#var-expansion)). The token is
        the file's own, as a literal one is, so the address needs no export
        ([Where a token may go](#where-a-token-may-go)).

=== "ansible.cfg"

    ```ini
    [galaxy]
    server_list = hub, galaxy

    [galaxy_server.hub]
    url = https://hub.example.internal/api/galaxy

    [galaxy_server.galaxy]
    url = https://galaxy.ansible.com
    ```

    ```bash
    export ANSIBLE_GALAXY_SERVER_HUB_URL=https://hub.example.internal/api/galaxy # (1)!
    export ANSIBLE_GALAXY_SERVER_HUB_TOKEN="${HUB_TOKEN}"
    go-galaxy install
    ```

    1.  The file's address again. A token you supply goes only to an
        address you supplied ([Where a token may go](#where-a-token-may-go)).

=== "Environment"

    ```bash
    export ANSIBLE_GALAXY_SERVER_LIST=hub,galaxy
    export ANSIBLE_GALAXY_SERVER_HUB_URL=https://hub.example.internal/api/galaxy
    export ANSIBLE_GALAXY_SERVER_HUB_TOKEN="${HUB_TOKEN}"
    export ANSIBLE_GALAXY_SERVER_GALAXY_URL=https://galaxy.ansible.com
    go-galaxy install
    ```

Each collection comes from the first server that has it: private ones from
`hub`, the rest from public Galaxy.

`lock` exits `5` when the hub names a `download_url` that carries a query
string or leaves the hub's origin. Such a hub works only without a lockfile
([Create the lockfile](lockfile.md#create-the-lockfile)). The path is the
hub's own, so a caching proxy's `.../get/<namespace>/<name>/<version>` locks.

## Which servers a run uses

The first source that is set decides:

1. `--server` or `GO_GALAXY_SERVER`. A value equal to an id in the list that
   steps 2 to 4 would pick selects that entry, with its token and TLS
   setting. Any other value is a URL, used with no entry's settings.
2. `ANSIBLE_GALAXY_SERVER_LIST`, those servers in order.
3. The `[[tool.go-galaxy.servers]]` entries of `galaxy.toml`, in order.
4. `[galaxy] server_list` in `ansible.cfg`, in order.
5. `ANSIBLE_GALAXY_SERVER`, one server.
6. `[galaxy] server` in `ansible.cfg`, one server.
7. `https://galaxy.ansible.com`.

An exported empty `ANSIBLE_GALAXY_SERVER_LIST` skips steps 3 and 4. An
exported empty `ANSIBLE_GALAXY_SERVER` skips step 6. A list entry brings its
own token and TLS setting ([Server settings](#server-settings)). Every other
setting follows
[Where a setting comes from](../reference/configuration.md#where-a-setting-comes-from).

## How a collection picks its server

go-galaxy asks each server in list order. On each server it tries the API
roots: paths under the server URL, such as `/api/v3` or `/v3`
([API roots and URL normalization](#api-roots-and-url-normalization)).

| The server answers                                                        | Result                                                      |
|:--------------------------------------------------------------------------|:------------------------------------------------------------|
| `200`                                                                     | The collection comes from this server                       |
| `404` at every API root                                                   | The next server is asked. With none left, the run exits `3` |
| A web page at one API root, as galaxy.ansible.com serves under `/v3`      | That root is skipped                                        |
| Web pages at every API root, like a single sign-on front                  | The run stops with exit `4`                                 |
| `401` or `403`                                                            | The run stops with exit [`4`](../reference/exit-codes.md)   |
| Any other status (`429`, `500`, `502`, `503` and `504` after the retries) | The run stops with exit `4`                                 |
| A document of the wrong shape, a refused connection, a DNS or TLS failure | The run stops with exit `4`                                 |

The server that answers owns the collection for the rest of the run. While
`install` or `lock` resolves, a failed later answer from it, for a versions
page or a version document, stops the run as the table shows. A `404` there
exits `3` instead: the server lacks the version or version list asked for.
After the resolve, while `install` installs, such a failure fails only that
collection and exits `5`
([When several things fail](../reference/exit-codes.md#when-several-things-fail)).

A [`source:`](requirements.md#collections) binds one collection to one
server. It names a server id, or a URL on a server's origin (scheme, host and
port), and the collection gets that server's token and TLS setting. An id is
matched exactly, case included, against the servers the run uses
([Which servers a run uses](#which-servers-a-run-uses)): a `--server` URL
leaves no id, and a `--server` id leaves only that one. An http(s) `source:`
that matches no server is still requested, with a warning and no token. Any
other `source:`, such as an id no server of the run has, a host name with no
scheme or an `ftp://` URL, exits `2` before any request, `--frozen` included,
naming the entry's place in the list. `tree`, `explain`, `hash` and `cleanup`
do not judge it. `galaxy.lock` records the server's URL for an id and the
`source:` URL for an origin match.

> [!NOTE]
> `source:` binds only that collection, not its dependencies: they walk the
> whole list. Give a dependency its own entry with `source:`, or put the hub
> first.
>
> === "galaxy.toml"
>
>     ```toml
>     [project]
>     collections = [
>       { name = "acme.app", source = "hub" },
>       { name = "acme.common", source = "hub" },
>     ]
>     ```
>
> === "requirements.yml"
>
>     ```yaml
>     collections:
>       - name: acme.app
>         source: hub
>       - name: acme.common
>         source: hub
>     ```

## Where a token may go

With one server, export `GO_GALAXY_TOKEN` or pass `--token`. An empty value
runs anonymously. With several servers, `--token` or `GO_GALAXY_TOKEN` exits
`2`, even when empty. Give each server its own token instead, as
[A private hub, then public Galaxy](#a-private-hub-then-public-galaxy) shows.

A `401` or `403` comes from the server and exits `4`: the server turned the
token down, or needed one and got none. Check the token's value and which
server it is set for
([How a collection picks its server](#how-a-collection-picks-its-server)).

A token you supply goes only to an address you supplied. go-galaxy refuses it
for a server whose address, or disabled certificate check, is set in
`ansible.cfg` or `galaxy.toml`:

| Token supplied by                                                                   | URL, or `validate_certs = false`, from a file | Neither from a file |
|:------------------------------------------------------------------------------------|:----------------------------------------------|:--------------------|
| You: `--token`, `GO_GALAXY_TOKEN` or `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`             | Refused, exit `2`                             | Sent                |
| The file: a `token` in the same section or entry, a `galaxy.toml` `${VAR}` included | Sent                                          | Sent                |

An address is yours when it comes from `--server=<url>`, `GO_GALAXY_SERVER`,
`ANSIBLE_GALAXY_SERVER`, `ANSIBLE_GALAXY_SERVER_<ID>_URL` or the default.
`--server=<id>` and `url = "${VAR}"` leave it the file's. The rule stops a
checked-out repository from choosing where your secret goes
([Security boundaries](../internals/boundaries.md#credentials-and-the-token-pairing-rule)
gives the reasoning). A `galaxy.toml` is trusted with every variable its
`[tool.go-galaxy]` table names, so export secrets only to runs whose
`galaxy.toml` you trust ([Trust model](security.md#trust-model)).

```text
✗ galaxy server token destination came from a configuration file: server "hub" (https://hub.example.internal:443) in ansible.cfg
```

| Message                                                                                                     | Fix                                                                                 |
|:------------------------------------------------------------------------------------------------------------|:------------------------------------------------------------------------------------|
| `galaxy server token destination came from a configuration file: server "<id>"`                             | Export `ANSIBLE_GALAXY_SERVER_<ID>_URL` with the identical address                  |
| The same, naming `server ""`: the `[galaxy] server` line                                                    | Export `ANSIBLE_GALAXY_SERVER` with the identical address, or pass `--server=<url>` |
| `galaxy server certificate verification was disabled by a configuration file for a token it did not supply` | Export `ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS` with the identical value         |
| `--token is ambiguous with a multi-entry server_list`                                                       | Give each server its own token, such as `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`          |

A `galaxy.toml` entry has a second fix: write the token into it, as
`token = "${HUB_TOKEN}"`, and stop supplying a token of your own. When the
file supplies both the `url` and `validate_certs = false`, export both. The
run reports the address first.

> [!CAUTION]
> Pass the token as `GO_GALAXY_TOKEN`, not `--token`: other local users can
> read a flag's value with `ps` while the run lasts
> ([Trust model](security.md#trust-model)).

<details markdown>
<summary>My server id contains a hyphen</summary>

A shell cannot `export` a name such as `ANSIBLE_GALAXY_SERVER_MY-HUB_URL`.
Set it through `env` or a container's `-e`, or rename the id with `_`.
`--server=<url>` works too, but drops the whole list and the entry's own token
and `validate_certs`: give the token as `GO_GALAXY_TOKEN`.

```bash
env 'ANSIBLE_GALAXY_SERVER_MY-HUB_URL=https://hub.example.internal/api/galaxy' go-galaxy install
```

</details>

## TLS and a private CA

Trust the hub's CA and leave `validate_certs` unset:

```bash
cat /etc/ssl/certs/ca-certificates.crt hub-ca.pem > hub-bundle.pem # (1)!
export SSL_CERT_FILE="${PWD}/hub-bundle.pem"
go-galaxy install
```

1.  The public roots on Debian and Ubuntu; on macOS, `/etc/ssl/cert.pem`.

This also covers git, url and signature fetches, which never honor
`validate_certs`. Trusting the CA never gets a token of yours refused
([Where a token may go](#where-a-token-may-go)). `SSL_CERT_DIR` names a directory of
certificates, as well or instead.

> [!WARNING]
> On macOS and Windows, setting `SSL_CERT_FILE` or `SSL_CERT_DIR` replaces the
> whole system store, so the bundle must also hold public roots such as
> github.com's.
> On Linux each replaces only its own default: the bundle file or the
> certificate directories.

`validate_certs = false` turns verification off for that server's origin only
and warns on every run, even under `--quiet`. A second warning follows when
that server also has a token. A token of yours is refused there unless you
exported that `validate_certs` yourself ([Where a token may go](#where-a-token-may-go)).

```text
! TLS certificate verification is DISABLED for Galaxy server "hub" (https://hub.example.internal:443); this run cannot detect a man-in-the-middle on that host
```

## Server settings

| Key                                             | `[galaxy_server.<id>]`                              | `[[tool.go-galaxy.servers]]`    | Variable                                    |
|:------------------------------------------------|:----------------------------------------------------|:--------------------------------|:--------------------------------------------|
| `id`                                            | The section name, listed in `server_list`           | Required                        | `<ID>`: the id upper-cased, `-` kept        |
| `url`                                           | Required                                            | Required                        | `ANSIBLE_GALAXY_SERVER_<ID>_URL`            |
| `token`                                         | Optional                                            | Optional, a literal or `${VAR}` | `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`          |
| `validate_certs`                                | `true`, `false`, `yes`, `no`, `on`, `off`, `1`, `0` | A TOML `true` or `false`        | `ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS` |
| `api_version`                                   | Only `v3`                                           | Refused                         | -                                           |
| `username`, `password`, `auth_url`, `client_id` | Refused: configure an API token instead             | Refused                         | -                                           |
| Any other key                                   | Warned about and ignored                            | Refused                         | -                                           |

Refused means exit `2` when the configuration loads. These are refused too:

- an id outside `^[A-Za-z0-9_-]+$`, or two ids equal ignoring case
- a `url` with a user or password
- a token for plain `http` to a host other than `localhost` or a loopback
  IP literal: a name is never looked up in DNS
- two servers on one origin with different tokens or `validate_certs`

An exported variable always wins: an empty `_TOKEN` clears the token, an empty
`_VALIDATE_CERTS` restores verification, and an empty `_URL` exits `2`. An id
with no section is built from its variables alone. `galaxy.toml` entries
replace the sections outright and are never merged with them.

<details markdown>
<summary>Exact error messages for server entries</summary>

```text
✗ unsupported galaxy_server key: configure a Galaxy API token instead: server "hub" key "username"
! unsupported key "timeout" in [galaxy_server.hub] ignored
✗ galaxy.toml: [[tool.go-galaxy.servers]] entry 1: unsupported requirements file format: unknown key "api_version" in [[tool.go-galaxy.servers]]
✗ galaxy.toml: [[tool.go-galaxy.servers]] entry 1: unsupported requirements file format: [[tool.go-galaxy.servers]] validate_certs is not a boolean
✗ galaxy.toml: [[tool.go-galaxy.servers]] entry 1: unsupported requirements file format: [[tool.go-galaxy.servers]] needs both id and url
✗ galaxy.toml: unsupported requirements file format: [[tool.go-galaxy.servers]] entry 2 repeats id "hub"
✗ galaxy.toml: unsupported requirements file format: [[tool.go-galaxy.servers]] is not an array of tables
✗ galaxy.toml: unsupported requirements file format: [[tool.go-galaxy.servers]] entry 1 is not a table
```

Most other refusals open with one of these:

```text
unsupported galaxy_server api_version
invalid galaxy server id
duplicate galaxy server id
galaxy server is missing its url
invalid galaxy server url
galaxy server url must not contain userinfo
invalid validate_certs value
galaxy server token configured for an insecure plaintext transport
conflicting token for the same galaxy server origin
conflicting validate_certs for the same galaxy server origin
```

</details>

### API roots and URL normalization

A server URL is trimmed, loses one pair of surrounding double quotes and its
trailing slashes, and must then be absolute. go-galaxy then tries these API
roots under it:

| `url` ends in                        | Appended to `url`, in order                |
|:-------------------------------------|:-------------------------------------------|
| `/api/v3`, `/api/v2`, `/v3` or `/v2` | Nothing: the `url` as written              |
| `/api`                               | `/v3`, `/v2`, then nothing                 |
| Anything else                        | `/api/v3`, `/v3`, `/api/v2`, `/v2`, `/api` |

Each root is tried with and without a trailing slash. A `404` or a web page
moves on to the next root
([How a collection picks its server](#how-a-collection-picks-its-server)).
Once a root answers, go-galaxy asks only that root on that server. A server
that answered `404` at every root for a collection is not asked for it again
in the same resolve or `lock`, even where an earlier run cached an answer
from it.

## Roles and the v1 role API

| Server               | Serves v1 roles?     |
|:---------------------|:---------------------|
| galaxy.ansible.com   | Yes, under `/api/v1` |
| Standalone Galaxy NG | Yes, under `/v1`     |
| Automation Hub       | No                   |

```mermaid
sequenceDiagram
    participant G as go-galaxy
    participant V as Galaxy v1 API
    participant H as github.com
    G->>V: Role record, with server token
    V-->>G: GitHub user and repository
    G->>V: Version list, with server token
    V-->>G: Tags
    Note over G,H: No Galaxy token past here
    G->>H: git fetch at the tag
    H-->>G: Commit, which galaxy.lock pins
```

A Galaxy role walks the server list like a collection. A server that has no
v1 or does not list the role is skipped. The first server that lists the role
owns it.

| Case                                                      | Exit                                    |
|:----------------------------------------------------------|:----------------------------------------|
| No server in the list serves v1                           | `2`: add galaxy.ansible.com to the list |
| No v1 server lists the role                               | `3`                                     |
| The owning server answers `404` for the role's versions   | `3`                                     |
| The owning server returns an unusable record or page link | `2`                                     |
| Any other failed answer                                   | `4`                                     |

A role takes no `source:` ([Roles](requirements.md#roles)), and a git role
never asks a Galaxy server. A `GO_GALAXY_GIT_<ID>_URL` binding for
`https://github.com` applies to the fetch
([Git sources and credentials](#git-sources-and-credentials)).

## Git sources and credentials

=== "HTTPS token"

    ```bash
    export GO_GALAXY_GIT_CREDENTIALS=forge
    export GO_GALAXY_GIT_FORGE_URL=https://gitlab.example.internal/platform # (1)!
    export GO_GALAXY_GIT_FORGE_USERNAME=gitlab-ci-token
    export GO_GALAXY_GIT_FORGE_PASSWORD="${CI_JOB_TOKEN}" # (2)!
    ```

    1.  Covers every repository under `/platform` on that host, and nothing
        beside it.
    2.  A personal access token or a CI job token goes in the password.

=== "SSH deploy key"

    ```bash
    export GO_GALAXY_GIT_CREDENTIALS=forge
    export GO_GALAXY_GIT_FORGE_URL=ssh://forge.example.internal # (1)!
    export GO_GALAXY_GIT_FORGE_SSH_KEY_FILE=/run/secrets/deploy_key
    export GO_GALAXY_GIT_FORGE_SSH_KEY_PASSPHRASE="${DEPLOY_KEY_PASSPHRASE}"
    export SSH_KNOWN_HOSTS=/run/secrets/known_hosts # (2)!
    ```

    1.  The binding names no user: the login, such as `git`, comes from the
        repository URL.
    2.  Holds the forge's host key, recorded in advance.

=== "SSH agent"

    ```bash
    eval "$(ssh-agent -s)"
    ssh-add /run/secrets/deploy_key
    export SSH_KNOWN_HOSTS=/run/secrets/known_hosts
    go-galaxy install # (1)!
    ```

    1.  No binding: with no bound key, the agent `SSH_AUTH_SOCK` names is
        used. With neither, the run exits `2`.

| Variable                                      | What it holds                                                                                  | Required?                      |
|:----------------------------------------------|:-----------------------------------------------------------------------------------------------|:-------------------------------|
| `GO_GALAXY_GIT_CREDENTIALS`                   | The ids to read, comma-separated                                                               | Yes: an unlisted id is ignored |
| `GO_GALAXY_GIT_<ID>_URL`                      | `https`, `ssh`, or `http` on loopback only; a host, optional port and path prefix              | Yes                            |
| `GO_GALAXY_GIT_<ID>_USERNAME`, `_PASSWORD`    | Basic auth for an http(s) host                                                                 | Both, for an http(s) binding   |
| `GO_GALAXY_GIT_<ID>_SSH_KEY`, `_SSH_KEY_FILE` | For an ssh host: the PEM text, or a path read at startup                                       | One, for an ssh binding        |
| `GO_GALAXY_GIT_<ID>_SSH_KEY_PASSPHRASE`       | The passphrase of an encrypted ssh key                                                         | No                             |
| `SSH_AUTH_SOCK`                               | The agent, for an ssh host with no bound key                                                   | When no key is bound           |
| `SSH_KNOWN_HOSTS`                             | Host keys, else `~/.ssh/known_hosts` and `/etc/ssh/ssh_known_hosts`                            | No                             |
| `ALL_PROXY`, `NO_PROXY`                       | A `socks5://` or `socks5h://` proxy for ssh, and its exceptions; other values connect directly | No                             |

A binding applies when scheme, host and port match exactly and the
repository path equals the binding's path or lies beneath it: `/platform`
covers `/platform/app`, not `/platform-tools`. The longest matching path
wins. Ids and values are read as in
[How git and url bindings are read](#how-git-and-url-bindings-are-read).

No `git` binary runs, so no credential helper, `~/.netrc`, `~/.ssh/config` or
git config is read. A repository URL carrying a credential is refused.
Certificates are always verified
([TLS and a private CA](#tls-and-a-private-ca)), and cross-origin redirects are
refused. No secret is printed or stored, and no flag takes one
([Security boundaries: git and url sources](../internals/boundaries.md#git-and-url-sources)).
Entries are spelled under [Collections](requirements.md#collections) and
[Roles](requirements.md#roles).

> [!NOTE]
> An unknown or changed ssh host key exits `4`: there is no trust on first
> use. Record the key with `ssh-keyscan` and check its fingerprint first.

## URL sources and credentials

```bash
export GO_GALAXY_URL_CREDENTIALS=gh,artifacts # (1)!
export GO_GALAXY_URL_GH_URL=https://github.com/acme
export GO_GALAXY_URL_GH_TOKEN="${GITHUB_TOKEN}"
export GO_GALAXY_URL_ARTIFACTS_URL=https://artifacts.example.internal/ansible
export GO_GALAXY_URL_ARTIFACTS_TOKEN="${ARTIFACTS_TOKEN}"
```

1.  Every id to read: the variables of an unlisted id are ignored.

`_URL` names an `https` origin (`http` only for loopback) and an optional path
prefix. The path matches by whole segments, as for git, and the longest prefix
wins. `_URL` and `_TOKEN` are both required, or the run exits `2`. `_TOKEN` is
sent as `Authorization: Bearer <token>`. Behind a caching proxy
(`https://front/<upstream-url>`), bind the front host. Ids and values are read
as in [How git and url bindings are read](#how-git-and-url-bindings-are-read).

| Redirect hop                                                      | `Authorization` sent?       |
|:------------------------------------------------------------------|:----------------------------|
| Into a bound origin and prefix                                    | Yes, that binding's token   |
| Into an unbound origin, such as object storage after a GitHub 302 | No                          |
| From `https` to `http`                                            | Refused: the download fails |

A url source never gets the Galaxy token or a relaxed TLS setting, even on a
configured server's origin. Bind the same token under `GO_GALAXY_URL_*` when
that host accepts it. Spell the entry as
[Collections](requirements.md#collections) or [Roles](requirements.md#roles)
shows.

## How git and url bindings are read

- An id matches `^[A-Za-z0-9_-]+$`, no two ids differ only in case, and
  `<ID>` is the id upper-cased.
- A variable exported empty counts as unset. An unknown variable under a
  listed id is warned about and ignored.
- `_URL`, `_USERNAME` and `_SSH_KEY_FILE` are trimmed. Passwords, keys,
  passphrases and tokens are taken verbatim.
- Two ids are refused when their `_URL` values agree once scheme and host
  are lower-cased, a default port is dropped and trailing slashes are cut.
- The first problem stops the load. The id list is checked, then each id in
  order, its `_URL` before its other variables, then collisions between ids.
  Git bindings are checked before url ones.
- A refusal names the variable, never the secret.

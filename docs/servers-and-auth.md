# Galaxy servers and authentication

Beyond a single `[galaxy] server`, go-galaxy understands ansible's multi-server
configuration surface, so a fleet of CI jobs can share one `ansible.cfg` with a
private Automation Hub and the public Galaxy both configured. A project on
`galaxy.toml` can carry the same list in its own file instead, as
`[[tool.go-galaxy.servers]]` entries held to the same rules.

## server_list and per-server sections

`[galaxy] server_list` (or `ANSIBLE_GALAXY_SERVER_LIST`, which wins outright
whenever it is set at all, even to an empty string) is a comma-separated list of
server ids. An id must match `^[A-Za-z0-9_-]+$` - a `.` would make the
`[galaxy_server.<id>]` section grammar ambiguous, and anything else is unsafe to
fold into an `ANSIBLE_GALAXY_SERVER_<ID>_*` variable name - and no two ids may
be equal or differ only in case, since both spellings would fold onto the same
environment-variable prefix. Either one is a hard config-load error. Each id
gets its own `[galaxy_server.<id>]` section:

```ini
[galaxy]
server_list = automation_hub, release_galaxy

[galaxy_server.automation_hub]
url = https://hub.example.internal/api/galaxy

[galaxy_server.release_galaxy]
url = https://galaxy.ansible.com
```

```bash
export ANSIBLE_GALAXY_SERVER_AUTOMATION_HUB_URL=https://hub.example.internal/api/galaxy
export ANSIBLE_GALAXY_SERVER_AUTOMATION_HUB_TOKEN=xxxxxxxxxxxxxxxx
go-galaxy install
```

The `_URL` export here is not redundant with the identical `url` the section
above already names: a token this run supplies is refused against a server
URL this run read out of a configuration file - the ansible.cfg, or the
galaxy.toml below - instead of from the operator - see [--token](#--token)
below for why. Naming the same address again through the environment moves it
onto the operator's own channel, which is what lets the token export above
resolve at all.

The same two servers in a `galaxy.toml`, as `[[tool.go-galaxy.servers]]`
entries beside the project's own dependencies:

```toml
[project]
collections = ["acme.app"]

[[tool.go-galaxy.servers]]
id = "automation_hub"
url = "https://hub.example.internal/api/galaxy"
validate_certs = true
token = "${HUB_TOKEN}"

[[tool.go-galaxy.servers]]
id = "release_galaxy"
url = "https://galaxy.ansible.com"
```

```bash
export HUB_TOKEN=xxxxxxxxxxxxxxxx
export ANSIBLE_GALAXY_SERVER_AUTOMATION_HUB_URL=https://hub.example.internal/api/galaxy
go-galaxy install
```

An entry is built through the same code as a `[galaxy_server.<id>]` section,
so everything this document says about a section - the id grammar, the per-id
environment overrides, the origin-conflict check, the TLS warnings,
`--server=<id>` selecting one entry, `--token` refusing a multi-entry list -
holds for an entry unchanged. `${HUB_TOKEN}` is expanded from the environment
by go-galaxy itself when the file loads (`${VAR}` is the one expansion form,
and a name that is not exported fails the run with the usage code; see
[galaxy.toml](configuration.md#galaxytoml)), and the `_URL` export is needed
for the same reason as above: a token that reaches the file through a
variable is yours, not the file's, while the `url` beside it is the file's,
so the pairing rule under [--token](#--token) refuses the pair until the
address is on your own channel as well. Three things differ from the
ansible.cfg shape. A non-empty entry list means the `[galaxy_server.<id>]`
sections of the ansible.cfg are not read at all: the two files are never
merged, and an id present in both takes its keys from the galaxy.toml alone.
The entries are the list: their ids, in file order, are the effective
`server_list` unless `ANSIBLE_GALAXY_SERVER_LIST` is exported, which outranks
them even when empty, and `[galaxy] server_list` is consulted only when the
galaxy.toml has no entries. And an id repeated exactly is refused when the
file loads (`[[tool.go-galaxy.servers]] entry 2 repeats id "automation_hub"`),
before any server is built, while two ids that differ only in case are
refused by the list rule above, exactly as in a `server_list`.

Every collection is resolved independently against `automation_hub` first, falling
back to `release_galaxy` only if the private hub doesn't have it - so one install
can legitimately draw some collections from the private hub and the rest from the
public Galaxy.

go-galaxy also auto-discovers the API root under `url`. It tries `/api/v3`
(galaxy.ansible.com's shape) and then the bare `/v3` a Galaxy NG / Automation
Hub deployment mounts directly under its own base path, so the same `url` works
for either shape without an extra option, and after those `/api/v2`, `/v2` and
a bare `/api`. Each root is tried with and without a trailing slash, and a
later root only when every earlier one answered 404. A `url` that already ends
in `/api/v3`, `/api/v2`, `/v3` or `/v2` is used as is, never doubled, and one
ending in `/api` gets `/api/v3`, `/api/v2` and `/api` itself. Once a root has
answered for one collection, later collections against the same server URL
probe only that root; a 404 never rules a root out, since it means only that
one collection is absent there.

`[galaxy_server.<id>]` keys, and what go-galaxy does with them:

| Key                     | Support                                                                                                                                                |
|:----------------------|:-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `url`                   | Supported, required. A token is never sent to a `url` supplied by this key unless this same section also supplies the token - see [--token](#--token). |
| `token`                 | Supported. Authorizes only itself against this section's own `url`; it does not authorize an operator-supplied token overriding it.                    |
| `validate_certs`        | Supported (see TLS below). A token is never sent over a connection this key disabled verification for unless this same section also supplies the token - see [--token](#--token). |
| `api_version`           | Accepted only as `v3` (a no-op; this tool always speaks the v3 API); any other value is a config-load error.                                           |
| `username`, `password`  | Hard config-load error naming the key: this is ansible's Basic auth, which this tool does not implement.                                               |
| `auth_url`, `client_id` | Hard config-load error naming the key: this is ansible's Keycloak/SSO token exchange, which this tool does not implement.                              |
| anything else           | Warned about and ignored.                                                                                                                              |

Basic auth and Keycloak/SSO are refused outright rather than silently sending an
unauthenticated request and surfacing a confusing 401 later - the error names the
offending key so you know to configure a plain API token instead.

A `[[tool.go-galaxy.servers]]` entry takes four keys, and its schema is closed
like every `galaxy.toml` table's:

| Key                     | Support                                                                                                                                                |
|:----------------------|:-----------------------------------------------------------------------------------------------------------------------------------------------------|
| `id`                    | Required, non-empty; the `server_list` id grammar and duplicate rule above apply to it.                                                                |
| `url`                   | Required, non-empty. File-sourced for the pairing rule even when written as `"${HUB_URL}"`: the variable picks the value, the file picked that a variable does. |
| `token`                 | Optional; an empty string is the same as none. A literal is the file's own token, exempt from the pairing rule as a section's `token` is. A `"${HUB_TOKEN}"` reference is your token, and the rule applies to it. A bare `$HUB_TOKEN` is not a reference and is sent as written. |
| `validate_certs`        | Optional, a TOML boolean: `true` or `false`. A quoted `"true"` is refused as `not a boolean`, and a bare `yes` is not TOML at all, so the file fails to parse. A `false` here is file-sourced for the pairing rule, as a section's is. |
| `username`, `password`, `auth_url`, `client_id`, `api_version` | Refused as unknown keys, where the ansible.cfg table above accepts `api_version = v3`, hard-errors on the other four and warns on anything unknown: ansible may read those keys in its own file, while nothing but go-galaxy reads a `galaxy.toml`, so there is no key to tolerate. |
| anything else           | Refused as an unknown key (`unknown key "x" in [[tool.go-galaxy.servers]]`).                                                                           |

An entry with no `id` or no `url` is refused too (`needs both id and url`).
That refusal and every refusal of a key inside an entry are prefixed
`[[tool.go-galaxy.servers]] entry N:`, N counting from 1 in file order; the
rest are spelled as quoted: `[[tool.go-galaxy.servers]] entry N repeats id
"hub"` for an exact duplicate id, and `[[tool.go-galaxy.servers]] is not an
array of tables` or `[[tool.go-galaxy.servers]] entry N is not a table` for a
servers value of the wrong shape. These are schema errors: the run exits `2`
when the file loads, before any server is built, and no value but a repeated
id is ever printed.

Every key also has a per-id environment override:
`ANSIBLE_GALAXY_SERVER_<ID>_URL`, `_TOKEN`, `_VALIDATE_CERTS`, where `<ID>` is
the id **upper-cased** and otherwise untranslated - a `-` stays a `-`, so an id
written `my-hub` is read from `ANSIBLE_GALAXY_SERVER_MY-HUB_URL`, and one
written `myHub` from `ANSIBLE_GALAXY_SERVER_MYHUB_URL`. That is ansible's own
rule, which composes the same name from the id upper-cased. The overrides
apply to a `[[tool.go-galaxy.servers]]` entry identically, key by key: the
entry is shaped into the same section before an override is consulted, so
`_URL` replaces its `url`, `_TOKEN` its `token` and `_VALIDATE_CERTS` its
`validate_certs`, exported-empty included. An id with no
`[galaxy_server.<id>]` section at all builds purely from its env overrides,
which is the common shape in containerized CI where you'd rather not template
an ansible.cfg for a secret. An override wins whenever it is exported, even as
an empty string: an empty `_TOKEN` clears the section's token, an empty
`_VALIDATE_CERTS` restores certificate verification, and an empty `_URL` leaves
the server with no URL at all, which is a configuration error.

A server URL is read the same way whichever channel supplies it - `--server`,
`GO_GALAXY_SERVER`, `ANSIBLE_GALAXY_SERVER`, `[galaxy] server`, a section's or
an entry's `url` or its `_URL` override: surrounding whitespace is trimmed,
one pair of surrounding double quotes is dropped (an exception to the
ansible.cfg rule that
[a quoted value keeps its quotes](configuration.md#what-go-galaxy-reads)),
and trailing slashes are cut. What remains must be an absolute URL with a
scheme and a host.

## --token

`--token` / `GO_GALAXY_TOKEN` is go-galaxy's own convenience for the common
single-server case - point the tool at one hub and hand it a credential without
writing an `ansible.cfg` section for it. It only applies when exactly one server
is effective (the built-in default, `[galaxy] server`, or `--server` - including
`--server` naming a single `server_list` id - or a one-entry list from either
file, whose entry then meets the pairing rule below with its file-sourced
`url`); with a multi-entry list in effect it is a hard error, since there is
no way to tell which server the credential belongs to - configure that
server's own `[galaxy_server.<id>]` or `[[tool.go-galaxy.servers]]` token
instead. When it does apply, it overrides that one server's own
configured token; setting it to the empty string clears the token entirely,
letting a pipeline force an anonymous run by exporting `GO_GALAXY_TOKEN=`
without editing any config.

The pairing rule below is documented here, but it is not scoped to this
flag. It applies to every token you supplied yourself, which includes a
per-server `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN` inside a multi-entry
`server_list`, and a `[[tool.go-galaxy.servers]]` `token` written as a
`${VAR}` reference - shapes `--token` itself cannot reach, since there it is a
hard error for the separate reason above. Every configured server is judged
on its own, and the first one that offends fails the whole config load
naming that server, so a `server_list` entry other than the first is refused
just as squarely. Where the rest of this section says "the single effective
server", read "the server that token is configured for" whenever the token
in your hands is a per-server `_TOKEN`.

**`--token` / `GO_GALAXY_TOKEN` is refused, not merely overridden, when the
single effective server's URL came from a configuration file - the
`ansible.cfg` or the `galaxy.toml` - rather than from you, and refused the
same way when that server's certificate verification was disabled by the
file rather than by you.** A repository can commit an `ansible.cfg` naming
any address it likes - a bare `[galaxy] server` line is enough, no
`server_list` and no `[galaxy_server.<id>]` section required - or a
`galaxy.toml` with a `[[tool.go-galaxy.servers]]` entry, and a token you
export the ordinary CI way must never follow an address you did not yourself
supply, nor cross a connection whose verification you did not yourself
disable. The two refusals read `galaxy server token destination came from a
configuration file` and `galaxy server certificate verification was disabled
by a configuration file for a token it did not supply`, each followed by the
server and the file that chose for it: `server "automation_hub"
(https://hub.example.internal:443) in galaxy.toml`, or the path of the
ansible.cfg. Two remedies both move the URL onto your own channel instead of
the file's, and neither needs a new switch: export `ANSIBLE_GALAXY_SERVER`
(or, for a list entry from either file, that id's own
`ANSIBLE_GALAXY_SERVER_<ID>_URL`) naming the identical address, or pass
`--server`/`$GO_GALAXY_SERVER` the address itself. Naming the list id
through `--server=<id>` does not help: an id match only selects which
section or entry applies, and its own `url` is still file-sourced either
way - and so is a `url` written as `"${HUB_URL}"` in a `galaxy.toml`, since
the variable picks the value but the file picked that a variable does, and
the rule judges who chose the destination, not who typed it. The TLS-policy
refusal has its own analogous remedy instead -
`ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS`, naming the identical value
already in the section or entry - since moving the *address* onto your own
channel does nothing for a *TLS policy* the file separately disabled for
it. When one section or entry supplies both `url` and `validate_certs`, the
two remedies are not alternatives: you need both, and you meet them one at
a time, since the destination refusal is reported first and the TLS one
only on the rerun after you have fixed it.

Whose token it is decides whether the rule applies at all. A `token` written
as a literal in either file is that file's own: it may follow the file's own
`url` and cross the file's own relaxed connection, since the author who chose
the destination also chose the secret, and nothing of yours is at stake. That
is allowed, not recommended: a literal in a committed file is readable by
everyone who can read the repository. A `token` written as `"${HUB_TOKEN}"`
in a `galaxy.toml` is yours: the file chose that a variable would supply the
secret, but the secret belongs to whoever exported the variable, so it is
judged exactly like `GO_GALAXY_TOKEN` or a per-id `_TOKEN` export, and the
file may not also choose where it goes. A section's or entry's own literal
`token` key does not open either door for a token of yours: overriding it
with `--token`/`GO_GALAXY_TOKEN` or the per-id `_TOKEN` while the `url` or
the `validate_certs` is still file-sourced is refused the same way, because
whether a file also declares a decoy `token` is a choice made by whoever
authored that file, not by you - the same pairing rule the two key tables
above state for `url`, `token`, and `validate_certs`.

A `server_list` id containing a `-` makes the `ANSIBLE_GALAXY_SERVER_<ID>_*`
remedy above unsettable by a plain shell `export`, since `-` is not a legal
character in a POSIX shell variable name. Four routes remain: `env
'ANSIBLE_GALAXY_SERVER_MY-ID_VALIDATE_CERTS=no' go-galaxy ...` (`env`
accepts a name a shell `export` cannot); a container's own `-e` flag, which
carries the same exemption; renaming the id in `server_list` or in the entry
to use `_` instead of `-`; or `--server=<url>` naming the address directly,
which discards the `[galaxy_server.<id>]` section or the entry entirely -
its own `token` and `validate_certs` go with it, so that route is a
different configuration rather than a workaround for this one.

Prefer the environment variable over the flag. A token passed as `--token`
lands in this process's argv, where any local process can read it - on Linux
through `/proc/<pid>/cmdline`, and in a `ps` listing on most systems - for as
long as the run lasts. `GO_GALAXY_TOKEN` carries the same value without that
exposure. go-galaxy does not detect which route you used and will not warn:
this is guidance about how you invoke the tool, not a check it performs.

## Precedence

Highest wins:

1. An explicit `--server` (or `$GO_GALAXY_SERVER`) collapses everything to one
   server: if its value matches a configured `server_list` id exactly, that
   server's own token and `validate_certs` apply; otherwise the value is used
   as an anonymous URL, and `server_list` plays no further part - not
   even to validate it.
2. Otherwise a non-empty server list wins, in list order. Three sources can
   supply the list, and the first one present decides it outright:
   `ANSIBLE_GALAXY_SERVER_LIST` whenever it is exported, even as an empty
   string, which then hides both of the others and leaves no list; else the
   `[[tool.go-galaxy.servers]]` entries of the `galaxy.toml` being
   installed, their ids in file order; else `[galaxy] server_list`. A
   `galaxy.toml` has no single-server key: a server it names is always a
   list entry, so a file with one entry is a one-entry list, which `--token`
   may set subject to the pairing rule, and whose section comes from the
   `galaxy.toml` alone.
3. Otherwise `[galaxy] server` from ansible.cfg, or `$ANSIBLE_GALAXY_SERVER`,
   which outranks that key whenever it is exported, even as an empty string:
   an exported but empty `ANSIBLE_GALAXY_SERVER` hides the key, and the run
   falls through to the built-in default below, not to the file's value. A
   `galaxy.toml` contributes nothing at this step.
4. Otherwise the `--server` flag's built-in default.

## Server selection at resolve time

Resolving a collection against `server_list` deviates from ansible in two
deliberate ways:

- **First match wins, not a union.** For an unpinned collection, go-galaxy walks
  the effective server list in order and installs from the first server that
  has it. Unlike ansible, which unions results across every configured server,
  go-galaxy never merges: a collection published on more than one server always
  comes from the earliest one that has it.
- **Fail closed, not fall through.** Only a 404 across all of one server's API
  root candidates means "this server doesn't have it, try the next one". A
  401/403 response, or a `429`/`500`/`502`/`503`/`504` that survives the retry
  budget, aborts the whole run naming the server that failed and exits with the
  network exit code (`4`) - it never silently advances to the next server. Any
  other failure aborts the run just as squarely and exits `4` too, but reports
  the underlying error rather than the named-server phrasing: another 5xx such
  as `501`, or a transport-level failure such as a refused connection, a DNS
  failure or a TLS handshake error, none of which is retried at all. A wrong
  token or a brief outage on your private hub must never quietly redirect an
  install to the public Galaxy instead.

A collection entry's `source:`, in `requirements.yml` or `galaxy.toml`, pins it
to one server for the whole run: an exact `server_list` id match, or a URL
matching a configured server's
network origin (so `source: https://hub.example.internal/content/published/`
still gets that server's own token and TLS policy, even though the path differs
from the configured `url`). The server URL a collection resolved against is
what the lockfile, the cache and the installed record name as its source, and
the server the install asks for the collection's download URL. A `source:`
naming a `server_list` id is recorded as that server's URL, never as the id,
and one matched by origin is recorded as its own URL, path included, not as
the configured `url`. A `source:` that matches no configured server is not
refused: it is requested anyway, with a warning naming it, and since a token
and a relaxed TLS policy follow a configured server's origin, that request
carries neither.

A `source:` pins only the collection that carries it. Its dependencies inherit
nothing from it and walk the whole server list, first match wins, like any
unpinned collection, so a dependency of a collection pinned to a private hub
comes from the first server that has it, which may be a public one listed
earlier. To keep a dependency on the hub, list it as a root with a `source:`
of its own, or put the hub first in `server_list`.

A network origin, here and throughout this document, is a URL's scheme, host
and port alone: scheme and host compare case-insensitively, a missing port is
the scheme's default (443 for https, 80 for http), and path, query and
fragment play no part. `https://H:443/` and `https://h` are therefore one
origin, while `https://h:8443` and `http://h` are two others. The same
comparison decides which configured server's token and TLS policy a request
carries, whether two configured servers share an origin, and which url
credential binding a request can match.

## Roles and the v1 role API

A Galaxy role (`owner.role` in the `roles:` list) is looked up through the
Galaxy **v1** role API, which is a different API from the v3 collection API
and is not served everywhere. galaxy.ansible.com serves it under
`<url>/api/v1/`, and a standalone Galaxy NG under `<url>/v1/`; both shapes are
probed, in that order, exactly as the v3 root is. Automation Hub serves no v1
API at all, and neither does a Galaxy NG behind the hub's API paths.

The walk is the collection walk: the effective server list in order, with the
first server that knows the role owning it. Two answers let the walk move on
to the next server - a 404 on every v1 root, meaning the server has no role
API, and an empty result, meaning it has one and does not list the role.
Everything else aborts the run as it does for a collection: a 401/403 and a
retryable 5xx that survives the retry budget name the server and exit `4`, any
other failure exits `4` reporting the underlying error. When the walk ends
without an answer the error says which of the two it was: no configured server
serves v1 at all is a configuration error naming the servers (exit `2`, the
remedy is to add galaxy.ansible.com or a standalone Galaxy NG to the list),
while servers that do serve v1 and none of them knows the role is a resolution
failure (exit `3`). A role entry takes no `source:` key; a role is never pinned
to one server.

Two requests answer a role: `roles/?owner__username=<owner>&name=<role>`
for the record, then `roles/<id>/versions/`, paginated, following the
server's own next link only while it stays on the server's origin and for at
most 20 pages. Both go through the same cache-policy-aware JSON fetch as the
v3 requests, on the Galaxy HTTP client, so the token configured for that
server is attached to them exactly as it is to a collection lookup, and to no
other origin. What the server answers is a pointer, never content: the
record's `github_user` and `github_repo` are held to the GitHub name alphabet
and composed into `https://github.com/<user>/<repo>`, `download_url` is not
read, and the role is then fetched from that repository at the chosen tag by
the git client - the same credential-free client every git collection uses,
which carries no Galaxy token, so the token configured for
galaxy.ansible.com never reaches github.com. A `GO_GALAXY_GIT_<ID>_URL`
binding for `https://github.com` (see the next section) does apply to that
fetch, should an operator configure one; without one the repository is
fetched anonymously, which is how `ansible-galaxy` downloads it too.

A git role names its repository directly and never touches a Galaxy server.

## TLS: validate_certs

`validate_certs = false` really disables certificate verification for that
server - but only for that one server's own network origin, never globally and
never for a download host on a different origin. The run warns loudly about
it, even in quiet mode (twice, if that server also carries a token, since the
token would then cross a connection this run cannot authenticate).

Prefer trusting a self-signed hub's CA over disabling verification at all:
point `SSL_CERT_FILE` or `SSL_CERT_DIR` at it and leave `validate_certs`
unset. That is the remedy that needs no `validate_certs` key at all, so
nothing below ever applies to it. Two things about those variables, both
Go's rather than this tool's. They *replace* rather than extend: on Linux,
`SSL_CERT_FILE` is read in place of the distribution's default bundle and
`SSL_CERT_DIR` in place of its default certificate directories, each on its
own; on macOS and Windows, setting either one sets the platform trust store
aside entirely and the run trusts exactly what the file and directory hold.
A bundle that is to stand alone must therefore also carry the public roots
the same run needs - github.com's, for a Galaxy role. And on macOS and
Windows they take effect in a go-galaxy built with Go 1.27 or later; an
older build consulted the platform store and ignored them, while Linux
builds have always honored them.

When `validate_certs = false` genuinely has to stay, and the same server
also carries a token you supply yourself (`--token`, `GO_GALAXY_TOKEN`,
that server's own `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`, or a `galaxy.toml`
`token` written as `${VAR}`), the pairing is refused unless the
`validate_certs` key is *also* sourced from your own environment rather than
the file, ansible.cfg or galaxy.toml alike: export
`ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS` naming the identical value. See
[Rejected as configuration errors](#rejected-as-configuration-errors) below
and [Galaxy servers and authentication](#galaxy-servers-and-authentication)
for the exact rule and its remedies.

## Git sources and credentials

A collection that comes from a git repository (see
[requirements.yml](configuration.md#requirementsyml) for the spellings), a git
role, and the repository a Galaxy role resolves to, are all fetched over
https or ssh by go-galaxy itself: no `git` binary is executed,
and therefore no credential helper, `~/.netrc`, `~/.ssh/config` or git
configuration of the runner is consulted. Whatever a private repository needs
is bound through the environment, and bound to a host, never written into
the requirements file.

```bash
export GO_GALAXY_GIT_CREDENTIALS=hub,forge

# an https host: Basic auth; a personal access token or a CI job token goes
# in PASSWORD, and USERNAME is whatever the host expects beside it
export GO_GALAXY_GIT_HUB_URL=https://gitlab.example.internal
export GO_GALAXY_GIT_HUB_USERNAME=gitlab-ci-token
export GO_GALAXY_GIT_HUB_PASSWORD="$CI_JOB_TOKEN"

# an ssh host: a private key, inline or from a file, with an optional
# passphrase; the ssh login comes from the repository URL (git@...)
export GO_GALAXY_GIT_FORGE_URL=ssh://forge.example.internal
export GO_GALAXY_GIT_FORGE_SSH_KEY_FILE=/run/secrets/deploy_key
export GO_GALAXY_GIT_FORGE_SSH_KEY_PASSPHRASE="$DEPLOY_KEY_PASSPHRASE"
```

`GO_GALAXY_GIT_CREDENTIALS` lists credential ids, under the same grammar as
`server_list` ids (`^[A-Za-z0-9_-]+$`, no two equal or differing only in
case). Each id is read from `GO_GALAXY_GIT_<ID>_*` with `<ID>` upper-cased.
`_URL` is required and names what the credential applies to: a scheme
(`https`, `http` or `ssh`), a host, an optional port and an optional path
prefix - `https://gitlab.example.internal/platform` covers every repository
under that group and nothing beside it. A credential applies to a requirement
URL only when the scheme, host and port match exactly and the path carries the
prefix; among several matching bindings the longest prefix wins. Nothing is
ever inferred from the requirement URL's own user, and a requirement URL that
carries a password of its own is refused outright, because it is repository
content.

The remaining keys decide the kind, and the kind has to fit the scheme:
`_USERNAME` and `_PASSWORD` together make a Basic credential for an http(s)
host (one without the other is refused; a token is a password here - GitHub
and GitLab both take it that way); `_SSH_KEY` (the PEM text) or
`_SSH_KEY_FILE` (a path, read once at startup), with an optional
`_SSH_KEY_PASSPHRASE`, make a key credential for an ssh host (both key forms
at once, or a passphrase without a key, is refused). A binding with neither
kind, two ids bound to one URL, or a Basic credential for a plaintext `http`
host that is not loopback, are refused the same way a Galaxy token on a
plaintext server is. An unknown `GO_GALAXY_GIT_<ID>_*` variable for a
declared id is warned about and ignored; variables for an id that is not
listed are ignored silently.

This surface and the url one [below](#url-sources-and-credentials) read their
variables by the same rules. A variable exported with an empty value counts
as unset. The non-secret values - `_URL`, `_USERNAME`, `_SSH_KEY_FILE` - are
trimmed of surrounding whitespace, so the trailing newline a CI secret store
may append cannot change them, while `_PASSWORD`, `_SSH_KEY`,
`_SSH_KEY_PASSPHRASE` and a url `_TOKEN` are taken verbatim, since a secret
may legitimately end in whitespace. Two ids are bound to one URL when their
`_URL` values agree once canonicalized - scheme and host lower-cased, a
default port dropped, trailing slashes cut - so `https://h/org` and
`HTTPS://H:443/org/` collide; that refusal is what keeps the longest-prefix
match from ever facing a tie. Problems are reported one at a time in a fixed
order - the id list, then each id in list order with its `_URL` first, then
two ids on one URL - and git bindings are checked before url bindings, so a
configuration broken in both reports the git failure first. A refusal names
the variable or id at fault and never echoes a secret.

An ssh repository with no bound key is reached through the agent
`SSH_AUTH_SOCK` names, and an ssh repository with neither a bound key nor an
agent is a configuration error, not a network one. The host key is checked
against `SSH_KNOWN_HOSTS` when that is set and against `~/.ssh/known_hosts`
and `/etc/ssh/ssh_known_hosts` otherwise; a host that none of them vouches
for, or whose key changed, fails the run - there is no trust-on-first-use,
and there is no flag to add one. An https repository's certificate is always
verified; `SSL_CERT_FILE`/`SSL_CERT_DIR` supply a private CA (replacing the
default trust store, see [TLS: validate_certs](#tls-validate_certs)), and no
`validate_certs` equivalent exists for git. A redirect that would move the
session to another scheme, host or port is refused rather than followed, so
a credential never travels anywhere but the origin it was bound to; write
the address the remote actually serves.

Every git secret is a `Secret` value like a Galaxy token: it never appears
in the snapshot, the lockfile, `GALAXY.yml`, the metrics file or any printed
line, and the lockfile records a repository URL, never a credential. Prefer
the environment over any file for the same reason the `--token` section
gives; there is deliberately no flag for a git credential.

## URL sources and credentials

A collection or role that comes from a direct http(s) tarball URL (see
[requirements.yml](configuration.md#requirementsyml) for the spellings) is
downloaded by go-galaxy itself, over a client of its own: no Galaxy token
and no relaxed TLS policy exists for any origin on it, so a requirements
file naming a configured server's own origin still gets neither. When the
host needs authentication - a GitHub release in a private repository, an
artifact store - a Bearer token is bound through the environment, to an
origin, never written into the requirements file:

```bash
export GO_GALAXY_URL_CREDENTIALS=gh,artifacts

# a GitHub personal access token for private release assets
export GO_GALAXY_URL_GH_URL=https://github.com/acme
export GO_GALAXY_URL_GH_TOKEN="$GITHUB_TOKEN"

# an artifact store, scoped to one path prefix
export GO_GALAXY_URL_ARTIFACTS_URL=https://artifacts.example.internal/ansible
export GO_GALAXY_URL_ARTIFACTS_TOKEN="$ARTIFACTS_TOKEN"
```

`GO_GALAXY_URL_CREDENTIALS` lists credential ids under the same grammar as
the git list above. Each id is read from `GO_GALAXY_URL_<ID>_*` with `<ID>`
upper-cased. `_URL` is required and names what the token applies to: a
scheme (`https`, or `http` for loopback only), a host, an optional port and
an optional path prefix. `_TOKEN` is required and is sent as
`Authorization: Bearer <token>`. A request carries the token only when its
scheme, host and port match the binding exactly and its path carries the
prefix; among several matching bindings the longest prefix wins. A binding
itself takes only an origin and a plain path prefix; a caching-proxy URL
(`http://front/<upstream-url>`) is covered by binding the front host's
origin, or a prefix the proxy is mounted under - the token authenticates to
the front host, never to the upstream the path embeds.

That decision is made again on every redirect hop. The ordinary GitHub
release shape - a 302 into presigned object storage - therefore works and
leaks nothing: the hop into the unbound origin carries no Authorization
header at all (an S3-shaped endpoint would refuse a request carrying both a
header and query signing anyway), and go-galaxy does not rely on the Go
HTTP client's own stripping heuristic, which forwards a pre-set header to
any subdomain of the host that set it. A redirect that leaves https for
plaintext http is refused outright. The URL itself may carry no credential
and no fragment; a query string is allowed and is part of the source's
identity, though every printed line cuts it.

A url token is a `Secret` value under exactly the git rules above: never
persisted, never printed, env-only with no flag. The Galaxy token is never
reused for a url source, even on the server's own origin - bind the same
value explicitly under `GO_GALAXY_URL_*` when a host takes it as Bearer.

## Rejected as configuration errors

These are refused before any request is made, exiting with the usage exit code
(`2`) - the operator has to fix `ansible.cfg`, an environment variable, or
the requirements file, not retry:

- A server URL, or a requirements file collection's `source:`, with embedded
  userinfo
  (`https://user:pass@hub/`).
- A server URL that is not absolute (no scheme or no host), or a `server_list`
  entry left with no URL, including one whose `ANSIBLE_GALAXY_SERVER_<ID>_URL`
  is exported empty.
- A token configured for a plaintext (`http://`) origin that isn't loopback.
  Loopback is judged from the URL as written, here and for the git and url
  bindings below: the name `localhost` or a loopback IP literal
  (`127.0.0.0/8`, `::1`). DNS is never consulted, so a name that resolves to
  loopback through `/etc/hosts` is refused.
- Two configured servers that share a network origin but disagree on their
  token or their `validate_certs`.
- A `server_list` id outside `^[A-Za-z0-9_-]+$`, or two ids that are equal or
  differ only in case - both spellings would fold onto one
  `ANSIBLE_GALAXY_SERVER_<ID>_*` prefix.
- A `[[tool.go-galaxy.servers]]` entry the file's schema refuses: no `id`
  or no `url`, a key outside `id`, `url`, `token` and `validate_certs`, a
  `validate_certs` that is not a TOML boolean, an id repeated exactly, or a
  `${VAR}` anywhere under `[tool.go-galaxy]` naming a variable that is not
  exported (`project file references unset environment variables:
  HUB_TOKEN`). Each is refused as the `galaxy.toml` loads, ahead of every
  other check in this list.
- An operator-supplied token (`--token`, `GO_GALAXY_TOKEN`, that server's
  own `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN`, or a `[[tool.go-galaxy.servers]]`
  `token` written as a `${VAR}` reference) paired with a server URL this run
  read out of `[galaxy] server`, a `[galaxy_server.<id>] url` or a
  `[[tool.go-galaxy.servers]] url`, unless that same section or entry also
  supplied the token itself as a literal (`galaxy server token destination
  came from a configuration file`).
- The identical operator-supplied token paired with a server whose
  certificate verification a `[galaxy_server.<id>]` or
  `[[tool.go-galaxy.servers]]` `validate_certs` key disabled, unless that
  same section or entry also supplied the token itself as a literal (`galaxy
  server certificate verification was disabled by a configuration file for a
  token it did not supply`). See [TLS: validate_certs](#tls-validate_certs)
  above for the remedy.
- A git credential binding that does not parse: an id outside the grammar or
  listed twice, a missing or malformed `_URL`, a kind that does not fit the
  URL's scheme, `_PASSWORD` without `_USERNAME`, both `_SSH_KEY` and
  `_SSH_KEY_FILE`, an unreadable or unparseable key, two ids bound to one
  URL, or a Basic credential for a plaintext non-loopback `http` host.
- A git requirement whose URL carries a credential, whose scheme is not
  `https`, `http` or `ssh` (or the `user@host:path` form), whose ref is an
  abbreviated commit or not a name git itself accepts, or whose `#subdir`
  is not a safe relative path; and an ssh repository reached with neither a
  bound key nor an agent. See [Git sources and
  credentials](#git-sources-and-credentials). A git role is held to the same
  URL and ref grammar, and additionally may carry no `#subdir` at all.
- A url credential binding that does not parse: an id outside the grammar or
  listed twice, a missing or malformed `_URL`, an `ssh://` or
  `user@host:path` binding, a query or fragment in the binding, a missing
  `_TOKEN`, two ids bound to one URL, or a token bound to a plaintext
  non-loopback `http` origin. See
  [URL sources and credentials](#url-sources-and-credentials).
- A url requirement whose URL carries a credential or a fragment, whose
  scheme is not `https` or `http`, whose path carries a dot or empty
  segment (the one exception is the `//` of a canonically spelled embedded
  upstream URL - the caching-proxy shape), or that names nothing beyond its
  origin; a `version:` beside it that is not exact; and a `source:`,
  `namespace:` or `signatures:` key on a url entry.
- A `roles:` entry this tool cannot install from: a non-http or
  non-`.tar.gz` URL, a local path, an `scm` other than `git`, an
  `include:`, a `source:`, `signatures:` or `type:` key, a name or version
  outside the role alphabets, or two entries installing into one directory;
  and a Galaxy role when no configured server serves the v1 role API, or
  when the record a server returns cannot be turned into a GitHub
  repository URL. See [Roles and the v1 role API](#roles-and-the-v1-role-api).

By contrast, an auth failure (401/403) or an unavailable server exits with the
network exit code (`4`) instead, since that's a runtime condition to retry or
investigate, not a configuration mistake.

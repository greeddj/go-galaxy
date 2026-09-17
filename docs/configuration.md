# Configuration

## What go-galaxy reads

`ansible.cfg` is discovered in ansible's own order - `$ANSIBLE_CONFIG`,
`./ansible.cfg`, `~/.ansible.cfg`, `/etc/ansible/ansible.cfg` - and parsed as
INI the way ansible parses it (CPython's `configparser`, with `;` as its only
inline comment marker), not as TOML. Three consequences follow from matching
ansible rather than a stricter parser: a quoted value keeps its quotes, so
`collections_path = "./c"` sets the literal `"./c"` and you should drop the
quotes; a `#` after a value is part of it, so
`server = https://galaxy.ansible.com # note` is a bad URL rather than a URL
with a note; and a `;` that follows whitespace starts a comment running to the
end of the line, so `server = https://galaxy.ansible.com ; note` is the URL
alone, while the `;` in `token = abc;def`, with no whitespace before it, stays
in the token.

Discovery keeps one of ansible's exceptions too: `./ansible.cfg` is not
considered at all when the current directory is world-writable, since any
other user on the machine could put a file there, and the run says so on
stderr rather than skipping it silently. The remaining candidates are still
tried. A container CI job whose workspace is `0777` therefore stops picking up
a workspace `ansible.cfg`; pass `--ansible-config` (or `$ANSIBLE_CONFIG`) to
name it explicitly, or tighten the directory's mode.

| Setting                            | Environment                                                   |
|:-----------------------------------|:--------------------------------------------------------------|
| `[defaults] collections_path`      | `ANSIBLE_COLLECTIONS_PATH`                                    |
| `[defaults] roles_path`            | `ANSIBLE_ROLES_PATH`                                          |
| `[galaxy] server`                  | `ANSIBLE_GALAXY_SERVER`                                       |
| `[galaxy] server_list`             | `ANSIBLE_GALAXY_SERVER_LIST`                                  |
| `[galaxy] cache_dir`               | `ANSIBLE_GALAXY_CACHE_DIR`                                    |
| `[galaxy] server_timeout`          | `ANSIBLE_GALAXY_SERVER_TIMEOUT`                               |
| `[galaxy_server.<id>]`             | `ANSIBLE_GALAXY_SERVER_<ID>_URL`, `_TOKEN`, `_VALIDATE_CERTS` |
| (the config file itself)           | `ANSIBLE_CONFIG`                                              |

`ANSIBLE_CONFIG` is the one row that is not a setting's environment override:
it names the file the other rows are read from, and it is a discovery
candidate rather than a strict source. A path in it that does not exist is
skipped in favor of the next candidate, whereas `--ansible-config` and
`$GO_GALAXY_ANSIBLE_CONFIG` name a file that has to be there - see
[install options](cli.md#install-options) for the two behaviors side by side.

`[defaults] roles_path` and `ANSIBLE_ROLES_PATH` are read exactly as their
`collections_path` counterparts: a `:`-separated search list whose first entry
is where roles install and whose remaining entries are named in one stderr
warning and otherwise ignored. `--roles-path` and `GO_GALAXY_ROLES_PATH`
outrank both, and the default is `.roles`, project-local like `.collections`
rather than ansible's `~/.ansible/roles`. A roles path that is the same
directory as the collections path is accepted with a warning.

Both of those warnings are printed only to a run that has roles to install,
which means a `requirements.yml` carrying a non-empty `roles:` block. A run
without one never reads `roles_path`, so how it was spelled cannot affect its
outcome, and the warning would be noise about a setting that went unused. The
`collections_path` warning has no such condition: every run installs into that
path.

One variable go-galaxy reads is deliberately absent from that table.
`ANSIBLE_GALAXY_REQUIREMENTS_FILE` sits in ansible's namespace without being an
ansible option: ansible-core declares no requirements-file setting, and
`ansible-galaxy` takes that path only as `-r/--role-file`. It is read anyway,
and it is not going away, because pipelines already set it; it is documented
here rather than in the table so that nobody expects `ansible-galaxy` to
honour it. The flag itself does port: `--role-file` is accepted as an alias of
`--requirements-file`, naming the one file that carries both the `collections:`
and the `roles:` lists.
`GO_GALAXY_TOKEN` is the other name with no ansible counterpart, for the
separate reason described under
[Galaxy servers and authentication](servers-and-auth.md#galaxy-servers-and-authentication).

A git collection source, and a git role, add a small environment surface of
their own, none of it with an ansible counterpart: `GO_GALAXY_GIT_CREDENTIALS` and the
`GO_GALAXY_GIT_<ID>_*` variables bind a credential to a repository host (see
[Git sources and credentials](servers-and-auth.md#git-sources-and-credentials)),
a url source likewise binds a Bearer token per origin through
`GO_GALAXY_URL_CREDENTIALS` and the `GO_GALAXY_URL_<ID>_*` variables (see
[URL sources and credentials](servers-and-auth.md#url-sources-and-credentials)),
and three variables go-galaxy reads rather than defines decide what an ssh
repository is reached with: `SSH_AUTH_SOCK` names the agent used when no key
is bound, `SSH_KNOWN_HOSTS` names the known_hosts file (`~/.ssh/known_hosts`
and `/etc/ssh/ssh_known_hosts` otherwise), and `SSL_CERT_FILE`/`SSL_CERT_DIR`
supply a private CA for an https repository exactly as they do for a Galaxy
server (replacing the default trust store, see
[TLS: validate_certs](servers-and-auth.md#tls-validate_certs)).
`~/.ssh/config` is not read. A Galaxy role is fetched from
`https://github.com/<user>/<repo>` by the same git client, so a
`GO_GALAXY_GIT_<ID>_URL=https://github.com` binding, when one is configured,
applies to it as well; the Galaxy token never does.

Anything else in `ansible.cfg` is ignored. Within `[galaxy_server.<id>]` the
exceptions are deliberate and loud: `username`/`password` (Basic auth) and
`auth_url`/`client_id` (Keycloak/SSO) are refused as config errors naming the
key rather than ignored, because silently dropping a credential would send an
unauthenticated request to a private hub. A token is also never paired with a
`[galaxy] server` or `[galaxy_server.<id>] url` this file supplied, and
never paired with a server whose certificate verification a
`[galaxy_server.<id>] validate_certs` key in this file disabled: for
`[galaxy] server` the destination refusal is unconditional, since there is
no `[galaxy] token` to satisfy it and no `[galaxy] validate_certs` key to
relax in the first place, while for a `[galaxy_server.<id>]` section either
refusal lifts only when that same section also supplied the token - see
[Galaxy servers and authentication](servers-and-auth.md#galaxy-servers-and-authentication) for
the full table, token precedence, and TLS.

## requirements.yml

```yaml
---
collections:
  - name: community.general
    version: "11.1.0"
  - name: ansible.posix
    version: "2.0.0"
    source: https://galaxy.ansible.com
```

A collection can also come from a git repository, in every spelling
`ansible-galaxy` accepts:

```yaml
---
collections:
  # auto-detected by the git+ or git@ prefix; version defaults to HEAD
  - git+https://github.com/acme/app.git
  - git@github.com:acme/app.git
  # explicit type with a ref: a branch, a tag, or a full 40-hex commit
  - name: https://github.com/acme/app.git
    type: git
    version: v1.2.0
  # ansible's combined form: "#<subdir>" selects a directory inside the
  # repository, ",<ref>" the ref, in that order, and the ref after the comma
  # wins over a version: key
  - git+https://github.com/acme/mono.git#collections/app,main
  # a repository holding one collection you name explicitly
  - name: acme.app
    type: git
    source: ssh://git@git.example.internal/acme/mono.git#collections
```

`version:` is the git ref to check out - a branch, a tag, a qualified
`refs/heads/...` or `refs/tags/...`, or a full forty-digit commit; an
abbreviated commit is refused. The collection's own version, namespace and
name come from its `galaxy.yml`, and the version there has to be an exact
`MAJOR.MINOR.PATCH`. A repository whose root carries no `galaxy.yml` is
searched one level down: every immediate child directory with a `galaxy.yml`
(or a built tree's `MANIFEST.json`) is a collection, and all of them are
installed unless the entry names one. A `signatures:` key is not accepted on a
git entry - the artifact is built here and nobody has signed it - and a
credential in the URL is refused; credentials are bound through the
environment (see
[Git sources and credentials](servers-and-auth.md#git-sources-and-credentials)).
A collection can also come from a direct tarball URL:

```yaml
---
collections:
  # auto-detected by the http(s) scheme; the artifact's own MANIFEST.json
  # names the collection and its version
  - https://github.com/acme/kafka/releases/download/0.24.0/acme-kafka-0.24.0.tar.gz
  # explicit type, with the version asserted against the manifest
  - name: https://artifacts.example.internal/ansible/acme-app-1.2.3.tar.gz
    type: url
    version: "1.2.3"
  # a caching proxy that reads the rest of the path as the upstream URL;
  # the embedded URL must be spelled in its canonical form
  - http://cacheproxy.mirror.example.com/https://github.com/acme/kafka/releases/download/0.24.0/acme-kafka-0.24.0.tar.gz
```

A `url` entry is downloaded directly over its own credential-scoped client
(see [URL sources and credentials](servers-and-auth.md#url-sources-and-credentials)),
its identity and dependencies read from the artifact's MANIFEST.json, and
pinned - in the cache key, the installed record and the lockfile - by the
origin bytes' sha256. A `version:` beside it must be exact and must match
the manifest, or the run fails; `signatures:`, `source:` and `namespace:`
are refused on a url entry, as are a credential or a `#fragment` in the URL.
The `file` and `dir` types stay unsupported. See
[Compatibility with ansible-galaxy](ansible-galaxy-compat.md) for what
differs from `ansible-galaxy` on a git or url source.

A bare top-level list is a list of collections here, where `ansible-galaxy`
reads it as the legacy roles format; roles always go under a `roles:` key. A
`src:` or `scm:` key inside a `collections:` entry is refused with a message
naming the `roles:` key, rather than silently installing nothing.

### roles

The `roles:` list takes every shape `ansible-galaxy`'s `role_yaml_parse`
accepts for a Galaxy role or a git role:

```yaml
---
roles:
  # a Galaxy role: owner.role, optionally ",<version>[,<install name>]"
  - geerlingguy.docker
  - geerlingguy.nginx,3.2.0
  - geerlingguy.pip,2.2.0,pip
  # the mapping form; version: a tag, absent or "*" for the highest
  - name: geerlingguy.java
    version: "2.3.0"
  # the old-style spelling
  - role: geerlingguy.git
  # a git role: the repository root is the role; version: is a ref and
  # defaults to HEAD; name: defaults to the repository name minus .git
  - git+https://git.example.internal/platform/ansible-role-base.git,v1.4.0,base
  - src: https://git.example.internal/platform/ansible-role-app.git
    scm: git
    version: release/1.x
    name: app
  - src: git@git.example.internal:platform/ansible-role-db.git
    version: 0123456789abcdef0123456789abcdef01234567
  # ansible's special case: an http(s) URL whose host is github.com, with no
  # scm and no .tar.gz suffix, is a git repository
  - src: https://github.com/acme/ansible-role-cache
    version: v2.0.0
  # a url role: an http(s) URL ending .tar.gz is downloaded directly and
  # pinned by its sha256; name: defaults to the basename minus .tar.gz, and
  # version: is only the label the role installs under (the sha's first
  # twelve hex digits when absent)
  - src: https://github.com/acme/ansible-role-cache/archive/2.0.0.tar.gz
    name: cache
    version: "2.0.0"
```

A Galaxy role name is `owner.role` with exactly one dot, each half matching
`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`; its `version:` is a tag the Galaxy server
lists (or the role's default branch), and an absent or `*` version means the
highest tag. A git role's `version:` is any git ref - a branch, a tag, a
qualified `refs/heads/...` or `refs/tags/...`, or a full forty-digit commit -
and defaults to `HEAD`. The install name (`name:`, the third comma field, or
the default derived from the Galaxy name or the repository URL) is the
directory the role lands in under `roles_path` and the name a playbook uses;
it must match `^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$` (no leading dot or hyphen)
and may never be `ansible_collections`. Quote a version that YAML would read
as a number (`"2.0"`), as the collection examples do.

Refused at load, with the usage code (`2`): `include:` (list the included
roles inline), an `scm` other than `git`, a non-http or non-`.tar.gz` URL or
a local path as `src:` (an http(s) `.tar.gz` URL is a url role, downloaded
directly and pinned by its sha256), a `#subdir` fragment on a git role (the
repository root is the role), a `source:`, `signatures:` or `type:` key on a
role entry,
two entries that would install into one directory, a name or version outside
the alphabets above, and a credential in a repository URL. An unknown key on a
role entry is warned about and dropped, where `ansible-galaxy` drops it
silently. A malformed `roles:` entry fails the whole file; earlier versions of
this tool never decoded the list at all and ignored it with a warning.

The dependencies a role's `meta/main.yml` and `meta/requirements.yml` declare
are installed transitively, in the same grammar, unless `--no-deps` is set. A
dependency with no dot and no scm is a local role `ansible-galaxy` never looks
up either and is skipped; a three-part `namespace.collection.role` is a
collection's role and is skipped with a warning pointing at the collection.

## ansible.cfg

```ini
[defaults]
collections_path = ./collections
roles_path = ./roles

[galaxy]
server = https://galaxy.ansible.com
cache_dir = /home/ci/.cache/go-galaxy
server_timeout = 60
```

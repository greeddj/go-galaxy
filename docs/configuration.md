# Configuration

## What go-galaxy reads

`ansible.cfg` is discovered in ansible's own order - `$ANSIBLE_CONFIG`,
`./ansible.cfg`, `~/.ansible.cfg`, `/etc/ansible/ansible.cfg` - and parsed as
INI the way ansible parses it (CPython's `configparser`, with `;` as its only
inline comment marker); the strict grammar [galaxy.toml](#galaxytoml) below is
held to is that file's alone. Four consequences follow from matching
ansible rather than a stricter parser: a quoted value keeps its quotes, so
`collections_path = "./c"` sets the literal `"./c"` and you should drop the
quotes; a `#` after a value is part of it, so
`server = https://galaxy.ansible.com # note` is a bad URL rather than a URL
with a note; a `;` that follows whitespace starts a comment running to the end
of the line, so `server = https://galaxy.ansible.com ; note` is the URL alone,
while the `;` in `token = abc;def`, with no whitespace before it, stays in the
token; and a section header's name runs to the last `]` on its line and is
taken as written, so `[galaxy] # prod` is the `[galaxy]` section, while
`[ galaxy ]` is a section named ` galaxy ` that neither ansible nor go-galaxy
reads as `[galaxy]`.

The parser follows `configparser` in a few more details. Whitespace is
whatever Python's `str.isspace` accepts, which adds the information
separators U+001C to U+001F to the usual set, and that one rule both trims
lines, keys and values and decides whether a `;` follows whitespace. A line
that reads as a section header stays one even with a delimiter after it, so
`[galaxy] = x` opens `[galaxy]`. Keys are lowercased while section names are
matched as written.

Unlike `configparser`, go-galaxy never refuses a file for its content. A
repeated key keeps its last value; a line that is neither a comment, a header
nor a key line is skipped, and such a line that opens with `[` also ends the
current section, so the keys below it are not read into the section above; a
leading UTF-8 byte order mark is stripped, where ansible refuses such a file
with `File contains no section headers`. The only parse failure is a read
error, a line longer than 64 KiB included, which exits `2` like an unreadable
file, whether the file was named or discovered; a discovered candidate that
vanished before it could be opened counts as no file at all.

Discovery keeps one of ansible's exceptions too: `./ansible.cfg` is not
considered at all when the current directory is world-writable, since any
other user on the machine could put a file there - one that sets
`collections_path`, `cache_dir` (the tree go-galaxy removes entries beneath)
and the servers a run fetches from. The sticky bit is no exemption, as in
ansible: it stops another user replacing an existing file but not creating a
new one, so a world-writable `/tmp` is skipped too. The run says so on stderr
rather than skipping the file silently, and says so whether or not an
`ansible.cfg` is there, since checking for the file first would keep every
run silent up to the one where it is planted. The remaining candidates are
still tried, and a current directory that cannot be resolved or examined
keeps the candidate.

A container CI job whose workspace is `0777` therefore stops picking up a
workspace `ansible.cfg`; pass `--ansible-config` (or `$ANSIBLE_CONFIG`) to
name it explicitly, or tighten the directory's mode. Only the implicit
candidate is dropped: a file named explicitly is read even when it is that
same file, a relative `ANSIBLE_CONFIG=ansible.cfg` included. The warning
states the rule rather than the outcome, so in that case only the `--verbose`
debug lines show which file a setting came from.

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
which means a requirements file (`galaxy.toml` or `requirements.yml`) carrying
a non-empty roles list. A run
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
honour it. It names the requirements file (`galaxy.toml` or
`requirements.yml`), and it may name a `.toml` file: the format is decided by
the extension of whatever path it holds, exactly as for `--requirements-file`,
and a value exported empty counts as a name (the empty path) rather than as
unset, so it also switches off the discovery described under
[galaxy.toml](#galaxytoml). The flag itself does port: `--role-file` is
accepted as an alias of `--requirements-file`, naming the one file that carries
both the collections and the roles lists.
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
`~/.ssh/config` is not read. An ssh repository is dialed through
`ALL_PROXY`/`all_proxy` as well: a `socks5://` or `socks5h://` value sends
every ssh fetch through that SOCKS proxy, except to a host
`NO_PROXY`/`no_proxy` matches, while any other scheme, or a value that does
not parse, means a direct connection. Both are read once per process, and the
repository host's key is still checked against known_hosts, since the ssh
handshake runs over the proxied connection. A Galaxy role is fetched from
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

### Environment variables and flags

Most `GO_GALAXY_*` variables, and several `ANSIBLE_*` and `AWS_*` ones, are
environment sources of a command-line flag, listed beside each flag in the
[CLI reference](cli.md). A value on the command line outranks them all, and
when a flag has several variables the first one that is set wins: a
`GO_GALAXY_` name comes ahead of an `ANSIBLE_` or `AWS_` one, and where a
flag has two `GO_GALAXY_` names, the one spelled after the flag itself comes
second, so `--timeout` reads `GO_GALAXY_SERVER_TIMEOUT`, then
`GO_GALAXY_TIMEOUT`, then `ANSIBLE_GALAXY_SERVER_TIMEOUT`. An integer flag's
variable exported empty counts as set but is not parsed, so
`GO_GALAXY_WORKERS=` leaves `--workers` at its default rather than failing
the run.

Two ansible variables are read apart from the flag they correspond to, since
a flag source would change their meaning. `ANSIBLE_GALAXY_SERVER` as a
source of `--server` would count as an explicit `--server` and collapse a
configured `server_list` to one server, where ansible treats it as the
`[galaxy] server` fallback (see [Precedence](servers-and-auth.md#precedence)).
`ANSIBLE_GALAXY_DISABLE_GPG_VERIFY` takes ansible's boolean spellings, which
a flag source would refuse, and is still read only by `install` and `warm`,
the commands that mount `--disable-gpg-verify` (see
[Turning it on](signatures.md#turning-it-on)).

## galaxy.toml

`galaxy.toml` is go-galaxy's own project file. `ansible-galaxy` cannot read
it, and nothing about `requirements.yml` changes: that file stays the drop-in
both tools read, in every shape the [next section](#requirementsyml)
describes, and a project keeps whichever of the two it prefers. What
`galaxy.toml` adds is a dependency grammar that puts the version constraint on
the same line as the name, a schema checked in full when the file loads, and a
file this tool is free to define. Today it holds the `[project]` table and
nothing else: no server, token, path or cache setting lives in it, and those
stay where [What go-galaxy reads](#what-go-galaxy-reads) puts them -
`ansible.cfg`, the environment and the flags.

```toml
[project]
name = "infra"
version = "1.0.0"
description = "My infra collections"
collections = [
  "sc.internal >= 0.0.20",
  "ansible.utils",
  "community.crypto >= 2.0, < 3.0",
  "community.general == 11.1.0",
  "acme.legacy ~1.5",
  "git+https://git.example.com/acme/mono.git#collections/app,main",
  "https://dl.example.com/acme-app-1.4.0.tar.gz",
  { name = "acme.app", version = ">= 1.4.0", source = "automation_hub" },
  { name = "acme.signed", version = "*", signatures = ["https://keys.example.com/a.asc"] },
  { name = "acme.net", type = "git", source = "https://git.example.com/acme/net.git", version = "v2.0.1" },
  { name = "https://dl.example.com/acme-lib-2.1.0.tar.gz", type = "url", version = "2.1.0" },
]
roles = [
  "geerlingguy.docker,7.4.1",
  "git+https://github.com/acme/ansible-role-nginx.git,v1.2.0,nginx",
  "https://dl.example.com/acme-role-1.0.0.tar.gz",
  { name = "postgres", src = "geerlingguy.postgresql", version = "3.5.0" },
]
```

### The `[project]` table

`[project]` takes five keys. `name`, `version` and `description` are optional
strings that describe the project to whoever reads the file: each is checked
to be a string and nothing more - `version = 1.0` is refused, since TOML reads
that as a number - and none of them is persisted anywhere, printed anywhere or
compared with anything, so a project's `version` neither has to be semver nor
has to move when its dependencies do. `collections` and `roles` are arrays of
strings and inline tables in any mix, or the array-of-tables spelling shown
below. At least one of the two keys must be present: a file that names
neither is refused, while `collections = []` is a valid file that installs
nothing. A file with no `[project]` table at all - an empty file, a
comments-only file - is refused the same way.

The schema is closed, unlike `requirements.yml`'s. Any other top-level table
or key (`[tool]`, `[servers]`, a `collections` array outside `[project]`), any
other `[project]` key, and any key on an inline table outside the sets listed
below is refused when the file loads, with the usage code (`2`) and a message
naming the key - never its value. The two files are held to different rules
because they belong to different tools. `requirements.yml` is ansible's file,
and a key this tool does not read there may be one `ansible-galaxy` does, so
that parser keeps ansible's tolerance (an unknown key on a role entry is warned
about and dropped). `galaxy.toml` is nobody's file but this tool's: a key it
does not know is a typo or a feature this build lacks, and both are things to
stop for rather than install past. That is also why a settings table is refused
rather than ignored: a file written for a build that reads one is never
silently half-read by a build that does not.

Values are literal. A `${VAR}` inside a string - a name, a constraint, a URL -
is not expanded today; write the value out.

### Dependency strings

A `collections` string is one of three things, judged in this order. A git
pointer (`git+...` or `git@...`), an http(s) URL, or anything shaped like a
path or another source (`./x`, `../x`, `/abs`, `~/x`, `ssh://h/r.git`,
`file:///x`) is handed whole to the rules the equivalent `requirements.yml`
entry gets: a git pointer keeps its `#subdir` and `,ref`, a URL is never cut,
and the path-shaped forms are refused as unsupported sources exactly as they
are in YAML. A bare name (`ansible.utils`) is the name alone at any version, as
a bare YAML string is. Everything else is a name followed by a constraint: the
name is the longest run of `A-Za-z0-9_.` at the start of the string, and what
follows must begin with a space, a tab or a version operator (`=`, `<`, `>`,
`!`, `~`, `^`, `*`), so `ns.name >= 1.0` and `ns.name>=1.0` are the same
requirement. A remainder that begins with anything else - `ns.name@1.0`,
`ns.name:1.0`, `ns.name-1.0` - is refused as an invalid collection name with
the hint `put a space or a version operator between the name and its
constraint`, since guessing where the name ends would install the wrong thing
silently. Upper case is deliberately inside the name run: `Acme.App >= 1` is
refused by the collection-name alphabet, as it is in YAML, rather than being
split at the first capital letter.

The constraint is what `requirements.yml`'s `version:` takes, so each spelling
means what the same `version:` string means there:

| Spelling                                               | Meaning                                                                                                       |
|:-------------------------------------------------------|:--------------------------------------------------------------------------------------------------------------|
| `ns.name`, `ns.name *`                                 | any version                                                                                                   |
| `ns.name 1.2.3`, `ns.name = 1.2.3`, `ns.name == 1.2.3` | exactly `1.2.3`; `==` is ansible's spelling of `=`, and a bare full triple is the exact pin, as in YAML       |
| `ns.name 1.0`                                          | the `~1.0` range, `>= 1.0.0, < 1.1.0`, as `version: "1.0"` is in YAML: a bare version missing a part is a range |
| `ns.name >= 1.0`, `ns.name>=1.0`                       | at least `1.0.0`; `>`, `<`, `<=` and `!=` likewise, with or without the space                                 |
| `ns.name >= 2.0, < 3.0`, `ns.name >= 2.0 < 3.0`        | both: a comma and whitespace are each an AND                                                                  |
| `ns.name ^1 \|\| ^2`                                   | either: `\|\|` is an OR                                                                                       |
| `ns.name ~1.5`, `ns.name ^1.2`                         | semver's tilde and caret ranges                                                                               |
| `ns.name 1.2 - 1.4`                                    | the hyphen range: from `1.2.0` up to and including every `1.4.x`                                              |
| `ns.name 1.x`                                          | the x-range, `>= 1.0.0, < 2.0.0`                                                                              |

A constraint's grammar is checked when the file loads. One the solver could not
parse (`ns.name >>= 1.0`) is refused as `invalid collection version
constraint`, naming the constraint and the collection, with the usage code
(`2`), before any root is resolved or any server is asked; the cache lock is
already held at that point, as it is for every read of the requirements file.
The same constraint in `requirements.yml` loads without complaint and fails
only once resolution reaches it, after every other root was prepared, with the
generic code (`1`), since that file's `version:` has always been
handed to the solver as written. Only the grammar is judged at load, never
whether a version exists: `ns.name >= 99` loads and fails in the solver, as it
does from YAML, and `ns.name1.0.0` is refused as a four-part name, not as a
constraint, since nothing separates the two.

### Inline tables and `[[project.collections]]`

An inline table is a `requirements.yml` mapping entry in TOML syntax, with the
closed key set `namespace`, `name`, `version`, `source`, `type` and
`signatures`. Once its keys pass, it is judged by every rule the
[requirements.yml](#requirementsyml) section states for the same mapping: a
`type = "git"` entry's `version` is a ref, a `type = "url"` entry's is an exact
version asserted against the manifest, and `signatures` and `source` are
refused where YAML refuses them. A Galaxy entry (no `type`, or
`type = "galaxy"`, with a name that is neither a git pointer nor a URL) has
its `version` checked as a constraint exactly as the string form is. Every one
of `namespace`, `name`, `version`, `source` and `type` must be a string:
`version = 1.0` is refused naming the key and the TOML type it was found to
be, never the value, since a TOML float `1.0` would print as `1` and read as a
range other than the one written. Keys are checked in sorted order, so a table
with two faults is always refused for the same one.

The array-of-tables spelling is the same list written one table per block;
TOML itself forbids spelling one key both ways, so a file that does is not
valid TOML:

```toml
[project]
name = "infra"

[[project.collections]]
name = "acme.app"
version = ">= 1.4.0"
source = "automation_hub"

[[project.collections]]
name = "acme.lib"

[[project.roles]]
name = "postgres"
src = "geerlingguy.postgresql"
version = "3.5.0"
```

### Roles

`roles` strings and tables are `requirements.yml`'s [roles](#roles) entries,
unchanged: a string is ansible's `src[,version[,name]]` form and is never
split, since its commas are field separators rather than a constraint, and a
table takes `name`, `role`, `src`, `scm`, `version` and `include`, with
`include` refused as it is in YAML. Two things are stricter than YAML: an
unknown key on a role table is refused rather than warned about and dropped,
and `name`, `role`, `src`, `scm` and `version` must be strings. A refused
roles list still leaves the collections readable, as it does from
`requirements.yml`, so the tolerance `cleanup` extends to such a file (see
[cleanup options](cli.md#cleanup-options)) holds for a `galaxy.toml` too.

### Discovery

Which file a run reads is decided before anything is opened, by a rule that
mirrors the strict-versus-discovery split `ansible.cfg` has. A file named
explicitly - `--requirements-file` (`-r`, `--role-file`),
`$GO_GALAXY_REQUIREMENTS_FILE` or `$ANSIBLE_GALAXY_REQUIREMENTS_FILE` - is
read as named, and its format is decided by its extension alone: a `.toml`
path, in any case (`.TOML` included), is parsed as `galaxy.toml`, and every
other path as `requirements.yml`, so `galaxy.txt` holding TOML is parsed as
YAML and refused with the usage code (`2`) - the example above as
`invalid collection name: "project"`, since YAML reads its `[project]` line as
a bare list holding one collection. Nothing sniffs the content. With none of the
three set, `./galaxy.toml` is read when it is a regular file (a symlink to one
counts), and `./requirements.yml` otherwise - that second candidate is not
examined at all, so a directory holding neither file fails exactly as it
always did, with `open requirements.yml: no such file or directory`. When
both files are present the run reads `galaxy.toml` and warns on stderr:

```text
galaxy.toml and requirements.yml are both present in the current directory; using galaxy.toml and ignoring requirements.yml (name one with --requirements-file to choose)
```

A `galaxy.toml` that exists but is not a regular file - a directory, a fifo -
is skipped with `./galaxy.toml is not a regular file and is ignored; reading
requirements.yml instead (name a file with --requirements-file to read it)`,
and the run continues with `requirements.yml`; a fifo is what the regular-file
rule exists to exclude, since opening one would park the run on whoever writes
to it. `install`, `warm`, `lock` and `outdated` print either warning with
their other configuration warnings, `hash`, `tree` and `explain` on their own
before their output. A variable exported empty counts as set:
`GO_GALAXY_REQUIREMENTS_FILE=` names the empty path, switches discovery off,
and the read then fails on that empty path as it did before discovery existed.
`cleanup` mounts no requirements flag and runs no discovery: it reloads the
path each project's registry record holds, by the extension that path carries.

Unlike `ansible.cfg` discovery, a world-writable current directory is no bar,
deliberately. That rule keeps the process from obeying a setting another user
planted, and a requirements file is not a setting the process obeys but the
thing it was asked to install: a planted `galaxy.toml` there could do nothing a
planted `requirements.yml` could not already do.

### What changes for a project that migrates

The registry records the absolute path of the file that was read, in the same
field a `requirements.yml` path always went into, so `cleanup` finds a project
by whichever file it installed from (an older binary sharing the cache fails
closed on that record instead; see the
[upgrade note](cli.md#cleanup-options)). The lockfile does not care which file
the roots came from: `galaxy.lock` is byte-identical for identical roots, and
`lock --frozen` passes across a migration that keeps every requirement the
same. The cached resolution is narrower. A constraint's spelling, its inner
whitespace included, is part of the requirements signature that decides
whether the last resolution is replayed, so a migration that respells a
constraint (`">= 1.0"` in YAML to `>=1.0` in TOML, or `"1.0.0"` to
`== 1.0.0`) re-resolves once against the servers and is replayed from then on,
while one that copies each constraint character for character replays at
once. `hash` with no lockfile hashes the raw bytes of whichever file was
picked, so that key changes with the migration; with a lockfile it does not.

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

A requirements file names each collection once, across every source kind.
Two entries with the same `namespace.name`, or the same git or url source,
are refused as a duplicate collection requirement with the usage code (`2`).
A git or url entry has no name until its `galaxy.yml` or `MANIFEST.json` is
read, so the check runs again once discovery has named every entry: a
`namespace.name` that two repositories produce, or a repository and a Galaxy
or url entry, is refused the same way rather than one of them silently
winning.

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

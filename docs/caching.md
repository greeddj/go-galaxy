# Caching

A run keeps four things between invocations: cached Galaxy API responses,
resolved dependency graphs and git and role pins (the snapshot), downloaded
and built tarballs (the artifact cache), unpacked collection and role trees
(the extracted store), and a registry of the projects that have run against
this cache. What each is keyed by, and why the
artifact cache is scoped by server rather than by content, is described in
[How it works](architecture.md).

## The local cache

By default everything lives under `$HOME/.cache/go-galaxy`, relocatable with
`--cache-dir` (`$GO_GALAXY_CACHE_DIR`, `$ANSIBLE_GALAXY_CACHE_DIR`). The
directory is decided in this order: the flag or one of its variables when set,
else `cache_dir` in the `[tool.go-galaxy]` table of the run's `galaxy.toml` (a
relative path resolved against that file's directory, a `${VAR}` expanded; see
[galaxy.toml](configuration.md#galaxytoml)), else `[galaxy] cache_dir` in
`ansible.cfg`, else the default. One run
holds it exclusively: a second run against the same directory fails fast with
`another instance is running` rather than interleaving writes. Installed files
are hardlinked out of the extracted store, so an installed collection and its
cache entry are one inode - which is what makes an install cheap and what makes
installed files read-only. See
[Differences a migration runs into](ansible-galaxy-compat.md#differences-a-migration-runs-into).

### What the directory holds

Five kinds of entry, and only one of them is a database:

| entry | what it is |
|:------|:-----------|
| `go-galaxy.db` | the snapshot, a single BoltDB file |
| `<fingerprint>.<name>-<version>.tar.gz` | the artifact store, flat files, most with a `.sha256` beside it |
| `extracted/<sha256>/` | the extracted trees, ordinary directories |
| `projects.json` | the project registry `cleanup` reads |
| `.go-galaxy.lock` | the lock one run holds exclusively |

The fingerprint an artifact carries is the first twelve hex characters of the
SHA256 of the server base it came from, so the same tarball fetched from two
servers is two entries and neither is ever served in place of the other. A
collection built from a git source is keyed by its source locator instead, as
described above, and one downloaded from a url source by its own locator,
`url+<url>#sha256:<hex>`.

The `.sha256` beside a tarball is a shortcut, not part of the entry, so not
every tarball has one. It is written only after the tarball is in place and
only for a well-formed digest, and a failure to write it is ignored, since a
missing one costs no more than a re-hash on the next fetch. A missing,
truncated or non-hex `.sha256` reads as no recorded digest, and deleting an
artifact removes its `.sha256` with it.

`go-galaxy.db` holds thirteen buckets: `meta` for the schema version, the
save stamps described below, the requirements hash and the server, and
twelve data buckets - `api_cache`, `deps_cache`, `versions_cache`,
`requirements`, `resolved`, `graph`, `installed`, `warmed`, `git_pins`,
`installed_roles`, `role_pins` and `url_pins`. Each holds JSON, and a value
that does not decode fails the load with an error naming its bucket and key
rather than loading as an empty entry nobody could tell from a real one. A
`meta` bucket with no `schema_version` loads as version 0 and is dropped and
rebuilt as outdated; a file with no `meta` bucket at all is read as a
brand-new database at the current version.

Two stamps in `meta` decide what a snapshot is evidence of. `last_snapshot`
is set by every save on either backend, and it is what says a snapshot was
persisted - not the existence of `go-galaxy.db`, which the local backend
creates on open whether or not anything is ever saved. `cleanup` and a
`--dry-run` run save only over a persisted snapshot, so neither writes an
empty one into a cache that was never saved. `content_recorded` is set by a
save that carries installed, warmed or installed-role records and kept by
every later save, and `cleanup`'s extracted-store sweep keys on it instead:
`lock` saves a snapshot with no such records, so on a cache whose snapshot a
schema bump dropped while its extracted trees survived, `last_snapshot` alone
would read as "nothing is installed or warmed anywhere" and let the sweep
delete the whole store. The key is written only once it is set, so a
snapshot from a binary that predates it reads as "nothing recorded", and an
older binary sharing the cache can only make the sweep do less.

The local backend reports `go-galaxy.db` as corrupt (exit `9`) only on
bbolt's own corruption errors - `ErrInvalid` (which also covers a file
truncated below its meta pages), `ErrVersionMismatch` and `ErrChecksum`; a
permission or mmap failure is never read as corruption. `ErrVersionMismatch`
concerns bbolt's on-disk format, not the snapshot schema version, so a
`go.etcd.io/bbolt` upgrade that changed the file format would make every
existing local snapshot exit `9` rather than be dropped as outdated; check
for that when bumping it.

Nothing tree-shaped is kept in the database. The dependency graph is a bucket
of records, but a collection's files are a real directory under `extracted/`,
and an installed collection is hardlinks pointing into that directory. On a
ten-collection cache the proportions come out as 8 MB of `go-galaxy.db`, 6 MB
of tarballs and 68 MB of extracted trees.

The snapshot used to be nine separate files named `go-galaxy-meta.db`,
`go-galaxy-graph.db` and so on. It is one database now; those names survive
only so that `--clear-cache` recognizes leftovers from an older binary and
reclaims them.

`go-galaxy hash` prints a deterministic `sha256:...` of the lockfile, or of
the requirements file (`galaxy.toml` or `requirements.yml`) when no lockfile
is present, for use as a CI cache key.
`go-galaxy warm` populates the caches without installing anything, and
`go-galaxy cleanup` removes cached collections and roles no registered project
reaches any more - both are described in the [CLI reference](cli.md#commands),
whose [cleanup options](cli.md#cleanup-options) section carries the
reachability rules (for roles: only a recorded roles path is scanned, only a
directory carrying this tool's extract marker counts) and the 30-day retention
the extracted-cache sweep applies to warmed entries.

Two caches must not be shared between principals holding different Galaxy
credentials; see [Security](security.md#security--trust-model) for why.

A collection built from a git source lives in the same caches under its own
key. The artifact key's scope is the source locator
`git+<url>#<subdir>@<commit>` rather than a server base, so a branch that
moves produces a new key and never overwrites the artifact built from its
previous commit - and, the other way round, the artifacts of superseded
commits stay in the artifact cache until `--clear-cache`, since nothing
references them and no sweep targets them. The snapshot records a git pin per
`(url, ref, subdir)`: the commit the ref resolved to and the collections that
commit held, which is what a rerun replays without contacting the remote. A
pin is keyed by the git line itself, so editing its URL, ref or subdir is a
new key; it is invalidated by `--refresh` (a commit ref is never
re-advertised, it is its own answer) and by `--clear-cache`, never by age and
never by an edit elsewhere in the requirements file. `--offline` replays a recorded pin or fails;
`--no-cache` fetches and builds once and hands the build straight to the
install phase without committing it. `warm` records a git collection under
`namespace.name@version` like any other, so two commits of a branch that did
not bump the collection's version share one warmed entry; the artifact cache
itself keeps both.

A url source follows the same shape one dimension simpler. The artifact
key's scope is the locator `url+<url>#sha256:<hex>`, so an origin that
starts serving different bytes produces a new key rather than overwriting
the old artifact. The snapshot records a url pin per URL - the sha256 the
URL served and the collection identity and dependencies its MANIFEST.json
declared - which is what a rerun replays without contacting the origin (a
url role's pin lives in `role_pins`, keyed `url\n<url>`, and additionally
records the version label; a `version:` naming another label misses the pin,
so the URL is downloaded again and the pin rewritten under the new label).
`--refresh` re-downloads the URL, since there is no cheaper probe than the
download itself: unchanged bytes keep the pin and the artifact key, changed
bytes become a new pin. Everything else - the `--offline` replay, the
`--no-cache` handoff, the warmed entry - behaves as it does for a git source.

A role lives in the same caches by the same rules, with a role-shaped key. Its
artifact - the deterministic `tar.gz` built from the repository tree - is
keyed by the locator `git+<url>#@<commit>` (no subdir: the repository root is
the role) and the filename `role.<name>-<version>.tar.gz`, so a role and a
collection built from one repository and commit share a scope without
colliding, and a role installed under two names or at two versions is two
artifacts. The snapshot carries two role buckets. `installed_roles`, keyed by
install name, records where each role was materialized, the locator it was
built from, its artifact digest, its version, its Galaxy name and the install
names of its dependencies; it is a record of content on disk, like the
installed-collections bucket, and is what `cleanup` keeps a role's extracted
tree by and purges a removed role's artifact through. `role_pins`, keyed by
the requirement line, records what that line resolved to: for a git role the
key is the repository URL and ref and the pin carries the commit, the concrete
version and the dependencies the meta declared; for a Galaxy role a second pin
keyed by the Galaxy name and the version asked for carries the repository and
tag the v1 API pointed at, the commit it recorded, and sits over a git pin
for that repository and tag. A rerun replays both without touching the v1 API
or the remote. The three kinds of role pin share the bucket under disjoint
keys: a git pin `<url>\n<ref>\n` and a Galaxy pin
`galaxy\n<name>\n<requested-version>` each carry two newlines, a url pin
`url\n<url>` exactly one, since a canonical URL carries none. A new kind of
pin, or a change to any of these shapes, must keep them apart, or two kinds
of pin can overwrite each other. A pin is invalidated by editing its own line
(a new key, or for a url pin a new `version:` label), by `--refresh` - which
re-asks the v1 API and re-advertises the ref, keeping the pin when the commit
is unchanged and the artifact still cached - and by `--clear-cache`, which
drops every role pin but leaves the installed-roles records alone; never by
age. `--offline` needs a recorded pin
and the cached artifact, else exits `4` before anything installs; under
`--frozen` the pins come from the lockfile instead, so an artifact missing
under `--offline` fails that role alone and the run exits `5`. `--no-cache`
fetches and builds once at discovery and hands the build straight to the
install phase. `warm` caches a role's
artifact and its extracted tree and records the warmed key as
`role:<name>@<version>`, kept apart from the collection keys so a role and a
collection sharing a `name@version` cannot overwrite each other's entry; the
extracted-store sweep keeps the trees of every installed role that is not
being removed, plus the warmed ones within their retention window. The
resolve-side snapshot reuse that replays a collection resolution while
the requirements file, the server list and `--no-deps` are unchanged is not
involved in roles at all: a `roles:` list is replayed through its pins, entry
by entry, so editing a role line re-resolves that role and nothing else.

Adding the two role buckets bumped the snapshot schema to version 8; the url
source pins (the `url_pins` bucket, and the url-source fields a `role_pins`
entry gained) bumped it to version 9, the current one. The policy is
drop-and-rebuild, so the first run of a new schema version against an existing
cache rebuilds its metadata caches cold (the artifact and extracted stores are
untouched), and an older binary sharing the same cache refuses the snapshot
with `unsupported snapshot schema version` (exit `2`) rather than dropping the
buckets it does not know on its next save - for a binary from before version 8
the role buckets, which would leave a later `cleanup` with no record of any
installed role.

## Freshness and retention

A cached Galaxy API response is keyed by the SHA256 of its URL and served only
when the stored URL matches and the body is not empty. A response to a
question that names no exact version, such as which version of a collection
is highest, is served with no request for ten minutes; past that it is
revalidated with a conditional GET carrying the stored `ETag` and
`Last-Modified`, and a `304` renews the entry and keeps its body. Such a
response whose fetch time lies in the future counts as expired too, since a
negative age would otherwise pass the freshness test forever. A response
about an exact version has no such lifetime and is served until retention
drops it. A stored body that no longer decodes is refetched without
validators, since a server still holding them would answer `304` and hand the
corrupt bytes back forever. The body is stored verbatim, so a presigned
`download_url` in it keeps its query - cutting it would turn every
cache-served download into a `403`. Anyone who can read a shared cache can
therefore use such a URL, but only until its presign expires, however long
the entry is kept. `lock` refuses to write such a URL into `galaxy.lock`,
which is committed (see [The lockfile](architecture.md#the-lockfile)).

`--refresh` does not reach an answer that already names an exact version (see
[install options](cli.md#install-options)), so bytes a server republishes
under a version this cache already holds are never requested, and a
collection with no sha256 pin is judged against the digest recorded when it
was first fetched. Keeping the first-seen bytes is the safe direction, but it
means `--refresh` does not act on a "we republished this version with a fix"
advisory. Only a sha256 pin - a lockfile under `--frozen`, or a url source's
locator - holds the bytes to a fixed digest on every install, cache hit
included, and so refuses different bytes served for a pinned version.

Retention is applied when a snapshot is saved, identically for both
backends, and never to the run's in-memory state. API, dependency and
version-list entries written more than 30 days ago are left out of the saved
snapshot, and so are warmed entries more than 30 days past their last warm;
`cleanup` applies the same warmed window when it decides what to keep. An
entry's age counts from when it was written: reading it does not renew it,
so an entry still in use is refetched 30 days after it was fetched, and only
an API response's `304` revalidation renews its stamp. A stamp in the future
counts as stale, not as fresh and not as an error, with no allowance for
clock skew: on a cache shared by machines whose clocks disagree, an entry
written by one running ahead is dropped and refetched. The installed
collections and roles, the graph, the requirements, the last resolution and
the three pin buckets are kept whole. For the installed records that is
deliberate: an install that finds its collection already in place skips it
without re-recording it, so an age window would expire a live project's
records and let `cleanup` sweep its extracted trees. The
accepted cost is that `cleanup`, which skips a project whose collections
tree no longer exists, keeps the extracted trees that project's records name
indefinitely.

## Clearing and cleanup

`--clear-cache` runs once the snapshot is loaded under the exclusive lock. It
empties the API, dependency and version-list caches and forgets every git,
url and role pin, then deletes the cached artifacts: on the local backend the
top-level tarballs, their `.sha256` files, leftover download temps and the
per-bucket snapshot files an older binary wrote; on S3 every object under
`artifacts/`. It keeps the installed and warmed records, the last
resolution, the extracted store, `go-galaxy.db`, `projects.json` and
`.go-galaxy.lock` - unlinking the lock file this run holds would let the next
run lock a fresh one, and two runs would hold the lock at once. It is skipped
with a warning under `--dry-run`, and a failure to delete ends the run with
the lock released, so a failed clear never blocks later runs on the same
cache.

Separately, every `install`, `warm` and `lock` run reclaims the download and
extract temps a killed run left in the cache directory, right after it takes
the exclusive lock: the lock is what proves no live run still owns them, so
the sweep must never move ahead of it. It runs under `--dry-run` too, since an
orphan temp says nothing about what is installed, and a failure only warns.

When `cleanup` removes an unreachable collection, it deletes every project's
copy of that `namespace.name@version`, then its artifact and its dependency
entry. Both of those keys are scoped by the server the collection resolved
from, which only the collection's installed record in the snapshot names;
with no record, `cleanup` leaves both rather than guess a key, so the
dependency entry ages out after 30 days and the artifact stays until
`--clear-cache`. Collections are removed in sorted order, so a run that stops
at a failure leaves the same partial result every time. `cleanup` also
deletes, reachable or not, any scanned collection's artifact still cached
under the flat key older binaries wrote before artifact keys were scoped by
server - the escaped `<namespace>-<name>-<version>.tar.gz` with no
fingerprint - since no lookup reaches that key any more. A candidate that happens to have the shape of a
scoped key is left alone: a namespace directory on disk may contain a dot, so
one named `<12 hex digits>.acme` would spell a live key.

## S3 Cache (optional)

When a bucket is configured - `--s3-bucket`, `GO_GALAXY_S3_BUCKET`, or `bucket` in the
`[tool.go-galaxy.s3]` table of the run's `galaxy.toml` - go-galaxy uses S3 as the cache backend.
Artifacts and cache metadata are stored in S3; collections are still installed locally.
The bucket is reached only over the network, so `--offline` (or `GO_GALAXY_OFFLINE`) beside a
bucket from any of those sources exits `2` with `--offline cannot be combined with an S3 cache
bucket: the S3 cache is reached over the network` while the configuration is built, before any
backend opens; an offline run needs the local cache. `cleanup` takes no `--offline`, so
`GO_GALAXY_OFFLINE` does not reach it.

Every S3 setting can be written in `galaxy.toml` instead of being passed on each run, the two
credentials as `${VAR}` references, so that the file names where the cache lives and the
environment supplies whose keys sign:

```toml
[tool.go-galaxy.s3]
bucket = "ci-galaxy-cache"
region = "eu-central-1"
prefix = "go-galaxy/"
endpoint = "https://s3.example.com"
access_key = "${CI_S3_ACCESS_KEY}"
secret_key = "${CI_S3_SECRET_KEY}"
```

The keys are `bucket`, `region`, `prefix`, `endpoint`, `access_key`, `secret_key` and
`session_token`, strings, and `path_style_disabled`, a TOML boolean (`true`, not `"true"`); any
other key is refused when the file loads (`unknown key "x" in [tool.go-galaxy.s3]`, exit `2`).
Precedence is decided key by key: the flag or one of its variables when set - a variable exported
empty counts as set and names the empty value - else the file's key, else empty. So an exported
`AWS_ACCESS_KEY_ID`, a spelling of `--s3-access-key`, outranks an `access_key` written out in the
file, while the `${CI_S3_ACCESS_KEY}` above reaches the file's key through a variable of the
operator's own naming. A `${VAR}` naming a variable the environment lacks fails the run before
any other source is read (`2`, `project file references unset environment variables:
CI_S3_ACCESS_KEY`, every unset name of the table once, never a value), so a runner missing its
keys stops at once rather than at the bucket. `path_style_disabled` follows the same rule against
its flag. The backend is selected by the bucket that results, whichever source supplied it, and
the `[tool.go-galaxy.s3]` keys a run took from the file are named on one `--verbose` line,
`Galaxy.toml <path> supplied: s3.bucket, s3.access_key, ...`, never their values.

Under the configured `--s3-prefix` the bucket holds four kinds of object:
`state/store.json.gz`, the snapshot as one gzipped JSON object; `state/projects.json`,
the project registry as plain indented JSON, each record built exactly as the local
`projects.json` builds it; `artifacts/<key>`, one object per tarball under the key the
local store uses as its filename, with its sha256 in `x-amz-meta-sha256`, which every
fetch checks against the downloaded bytes; and `locks/cache.lock`, the distributed lock,
beside the short-lived `locks/.conditional-probe-<random>` objects `Open` writes to test the
endpoint. Both state objects are read under the size caps
[Security](security.md#security--trust-model) describes, which matter more here than on the
local backend: the read runs while the distributed lock is held, so an oversized object
would hold up every runner sharing the bucket, not only the one reading it.

Both credentials are mandatory: a bucket from any source without both an access key and a
secret key from any source exits `2` with `s3 cache requires access and secret keys when an S3
bucket is configured`.
Requests are signed by go-galaxy's own SigV4 implementation rather than by the AWS SDK, so
there is no credential chain behind those two values - no IAM role or instance profile, no
`~/.aws/credentials`, no `AWS_PROFILE`. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and
`AWS_SESSION_TOKEN` are read as alternative spellings of the [S3 cache
options](cli.md#install-options), and nothing else about an AWS environment is consulted.

Pass the two credential values through the environment rather than the command line.
`--s3-secret-key` and `--s3-session-token` land in this process's argv, where any local
process can read them for as long as the run lasts; `GO_GALAXY_S3_SECRET_KEY` /
`AWS_SECRET_ACCESS_KEY` and `GO_GALAXY_S3_SESSION_TOKEN` / `AWS_SESSION_TOKEN` carry the
same values without that exposure. This is guidance about how you invoke the tool, not a
check it performs - go-galaxy does not detect which route you used and will not warn.
`--s3-access-key` is deliberately not in this list: an AWS access key id travels in
cleartext inside every signed request's `Authorization` header by construction, so hiding
it from argv would prevent no disclosure.

A bucket the file chose, signed with keys the environment supplied, is accepted, where the
Galaxy side refuses a token from the environment for a server address a file chose (see
[--token](servers-and-auth.md#--token)). The two credentials are not the same kind of thing. A
Galaxy token is a bearer secret: it travels in the request, so whoever chooses the destination
chooses who holds it. SigV4 sends no secret. Each request carries an HMAC-SHA256 signature
computed from the secret key over the request itself - its method, path and query, its `Host`
among the signed headers, and a scope of date, region and service - and the secret never leaves
the process. An endpoint the file pointed the run at receives the access key id, which rides in
cleartext in every signed request anyway, a session token if one is configured, which is as
useless without the secret as the id is, and signatures valid only for those requests to that
host, from which the secret cannot be recovered. What a `galaxy.toml` decides is where the cache
lives, the decision it already makes for the local backend through `cache_dir`, while the
environment still decides whose keys sign. That holds for the choice of endpoint alone: a
`${VAR}` reads any variable exported to the run into any string of the table, the endpoint and
the prefix included, so a `galaxy.toml` is trusted with every variable the environment exports
to the run, and which variables those are is the responsibility of whoever prepares the
environment (see [Security](security.md#loading-requirementsyml-and-the-lockfile)). What the
file can still do is aim keys the runner holds at another bucket those keys can reach, so give
a project keys scoped to its own bucket, as any CI credential is scoped.

**The endpoint must support conditional writes - both of them.** The distributed lock that
keeps concurrent runs off each other's cache is built on `If-None-Match: *` to take the
lock and `If-Match` against an object's ETag to take over one whose holder died, so an
endpoint providing either one only nominally cannot back it. Amazon S3 supports both;
an S3-compatible implementation may not, and versions predating conditional-write support
do not. `Open` proves it rather than assuming it: on every run it writes a throwaway probe
object and checks that a create-if-absent write is refused when the key exists, that reads
name an ETag, that a write conditioned on a stale ETag is refused, and that one
conditioned on the current ETag is accepted. A backend failing any of those exits `2` with
`cache backend cannot be used as configured` - no retry helps, and the remedy is a
different endpoint. The last check matters most for an implementation that refuses every
`If-Match` alike: nothing about it looks permissive, and without that check it would pass
here and instead leave a dead holder's lock unreclaimable, which every waiting run reads
as ordinary contention.

The lock object carries its token, deadline and owner in `x-amz-meta-*` headers, which are
all the protocol reads, and mirrors them in a JSON body for a human reading the object.
The body still matters. For a plain single-part upload, Amazon S3's ETag is the MD5 of the
body alone and ignores the metadata headers, so two runs reclaiming the same expired lock
with `If-Match` on the same ETag are told apart only because each writes its own token
into the body: the first swap changes the ETag and the second is refused with `412`. A
constant or empty body would let both swaps succeed, and only the ownership check after
the write and the heartbeat would notice, once both runs had briefly believed they held
the lock. The in-process S3 double the tests use mints a distinct ETag for every write
whatever its body, so the tests would not catch that regression.

The lock is granted for ten minutes, and a live holder's heartbeat renews it every three,
each heartbeat's HEAD and PUT bounded together by 30 seconds. A waiter retries with
full-jitter backoff between 250 ms and 5 s and gives up after five minutes; a backend that
refuses the create with `412` while HEAD reports no object gets at most eight immediate
retries before that backoff applies. Release runs on a fresh context with its own 30
seconds, so a run that failed or was canceled still gives the lock back. Each cache-state
operation - loading or saving the snapshot, loading the project registry, recording the
project (a GET and a PUT under one budget) - runs under the lock with the fixed 60-second
ceiling the [install options](cli.md#install-options) describe, and a run performs at
most three of them while it holds the lock. The timings must therefore keep that ceiling
below the heartbeat interval, the heartbeat interval below the lock's lifetime, and three
ceilings below the wait limit, or the state budget starves the lock it runs under;
`TestStateObjectDeadlineFitsInsideTheLockTimings` pins those relations, and a change to
any of these constants must keep them. A save that runs out of time fails closed and
leaves nothing half-written, since an S3 PUT replaces an object whole or not at all.

A holder that dies of a Go fatal error - a stack overflow, running out of memory, a
runtime throw - runs no cleanup, so its lock blocks every runner on the bucket until its
ten minutes pass, and a runner arriving while more than five of them remain exits `8`
without reaching its work. The lifetime is not shortened for that case, since a shorter
one would let a live but slow holder be reclaimed.

Every idempotent S3 request - GET, HEAD, DELETE, a listing, a batch delete and a PUT with
no precondition - is retried, four attempts at most with full-jitter backoff between
200 ms and 5 s, and only for a stalled read, a transport failure while the run's own
context is still live, or a `429`, `500`, `502`, `503` or `504` - the same status set the
Galaxy requests retry. Anything else is final, an oversized response included. A transport failure is judged by whether the run's context has ended, never by
the error's shape, since a dial timeout and a response-header timeout also read as a
deadline, and a shape check would stop retrying the two commonest outages - an endpoint
that drops every packet and one that accepts a connection and never answers. A conditional
PUT is sent once, since it may have landed and a retry would read its own success as
contention, and so is bucket creation. A request made inside a budget (a state operation,
an artifact download, the lock wait) is cut short by it; one made outside any - the bucket
check at open, an artifact presence check, `--clear-cache`'s listing and deletes - can
take four attempts plus the backoff before it fails.

A run against the S3 backend distinguishes four ways the cache can fail to serve it. A
bucket that cannot be reached, or that answers a request with a failure of its own, exits
`4` - retry once the outage clears. A bucket that parses but cannot back the distributed
lock's guarantees (it does not enforce one of the two conditional writes), or an
`--s3-endpoint` that parses to no host, exits `2` - no retry helps; the configuration itself
has to change. An endpoint that is not a URL at all - an unclosed `[` in an IPv6 host, a
non-numeric port - fails earlier still, on the parse itself, and is reported unclassified
as exit `1` rather than joining this class. A bucket whose lock this run does not acquire
before its own wait ceiling elapses exits `8` only when this run actually saw another acquirer holding that lock
during the wait; a wait that reached the bucket but never got that answer - an endpoint
that never replies, replies only with failures, or contradicts itself - exits `4` instead,
alongside the other unreachable-backend cases. And a lock this run does acquire and then
loses - a later heartbeat finds another acquirer's token on the lock object - exits `8`
too: the run stops there instead of finishing its writes against a cache it no longer has
to itself. See [Exit codes](exit-codes.md) for the exact messages to grep for.

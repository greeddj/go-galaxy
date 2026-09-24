# Security

## Verifying a release

Every release is signed, catalogued and attested by the workflow that built
it. There is no public key to fetch and no key for anyone to lose: cosign
signs keylessly, so the identity in the certificate *is* the release workflow,
proved by a short-lived OIDC token from GitHub.

`checksums.txt` lists every asset by sha256, and it is what gets signed - so
verifying one signature and one hash covers whichever asset you actually
downloaded:

```bash
tag=v1.2.3
base="https://github.com/greeddj/go-galaxy/releases/download/$tag"
curl -sSLfO "$base/checksums.txt" -O "$base/checksums.txt.sigstore.json"

cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp \
    '^https://github\.com/greeddj/go-galaxy/\.github/workflows/release\.yml@refs/tags/'

sha256sum --ignore-missing -c checksums.txt   # or: shasum -a 256 --ignore-missing -c
```

Container images are signed the same way, with the signature stored in the
registry next to the image, so nothing needs downloading first:

```bash
cosign verify ghcr.io/greeddj/go-galaxy:latest \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp \
    '^https://github\.com/greeddj/go-galaxy/\.github/workflows/release\.yml@refs/tags/'
```

A signature says the file is the one that was published. Provenance says which
workflow, at which commit, produced it - a different question, recorded as a
GitHub build attestation for every asset:

```bash
gh attestation verify go-galaxy-linux-amd64 --repo greeddj/go-galaxy
```

Every build also ships an SPDX SBOM listing the Go modules actually linked into
it, for scanning against a vulnerability feed without unpacking anything. The
two published shapes name theirs differently, and the raw binary is the one
that surprises: an archive's SBOM sits next to it as `<asset>.sbom.json`
(`go-galaxy_<version>_Linux_x86_64.tar.gz.sbom.json`), while a raw per-platform
binary is renamed for the `releases/latest/download/` flow but its SBOM is not,
so the document for `go-galaxy-linux-amd64` is published as
`go-galaxy_<version>_linux_amd64.sbom.json` and `go-galaxy-linux-amd64.sbom.json`
does not exist.

macOS builds are **not** Apple-notarized, so Gatekeeper has nothing to check
them against - the signature and provenance above are what to verify instead.
Notarizing would need an Apple Developer ID this project does not have.

That has a consequence worth stating rather than burying. A binary downloaded
by a browser arrives quarantined, and a quarantined copy of this one does not
run. The Homebrew cask therefore clears the attribute from what it stages, in
a post-install hook that runs `xattr -dr com.apple.quarantine` - a packager
skipping a Gatekeeper check for you, which is the honest description of it.
The alternative was no cask at all, since one without the hook installs
something that will not start. Nothing else the release publishes touches the
flag: the raw binaries, the archives and the container images are handed over
exactly as built. If you would rather Gatekeeper stayed in the loop, install
by another route and verify the signature and provenance above; the cask
carries the same bytes as the archive it is built from, and those bytes are
listed in the same `checksums.txt`.

## Security / Trust model

- Every secret this tool accepts has an environment route, and that is the route to
  use. `--token`, `--s3-secret-key` and `--s3-session-token` all put their value in
  this process's argv, readable by any local process for the life of the run;
  `GO_GALAXY_TOKEN`, `GO_GALAXY_S3_SECRET_KEY` / `AWS_SECRET_ACCESS_KEY` and
  `GO_GALAXY_S3_SESSION_TOKEN` / `AWS_SESSION_TOKEN` do not. go-galaxy cannot tell the
  two routes apart and issues no warning: a value's source is not observable once
  urfave has resolved it, so a warning would have to fire on every run, including the
  environment-driven majority it exists to encourage.
- The shared S3 snapshot object and the project registry object are a trust boundary:
  go-galaxy serves cached Galaxy metadata (including a collection's download URL and
  sha256) from them without re-validating against the origin on every use, so anyone
  who can write to the bucket can influence what a run installs. Restrict bucket write
  access (a write-restricted ACL, or dedicated credentials) just as you would protect
  a local cache directory - the local Bolt snapshot is implicitly trusted for the same
  reason, since writing it already requires local filesystem access to the cache
  directory.
- On the shared S3 cache, the backend's exclusive lock is held for a whole run, so its
  hold time is proportional to the work that run requests: a large legitimate install
  holds it for as long as the install takes, and a run pointed at a slow or hostile
  Galaxy server - including one named by a requirements.yml `source:` that matches no
  configured server - can hold it far longer, while other runners sharing the bucket
  give up waiting and fail. A principal who can run against the shared bucket already
  holds bucket write credentials: a run against the S3 cache writes to it regardless of
  what it is asked to do - artifacts, the snapshot, the project registry - and the lock
  itself is taken by writing an object, so a read-only credential cannot run at all.
  Restricting bucket write access - the same guidance as the bullet above - is what
  bounds this too. Give untrusted runs (for example, CI jobs building from an untrusted
  branch or fork) their own bucket, or a prefix of their own together with a credential
  restricted to that prefix, or no S3 cache at all, rather than shared-bucket write
  credentials. `--s3-prefix` on its own is not a boundary: it selects which keys a run
  reads and writes, not which keys its credential may touch, so a run holding a
  bucket-wide credential can still write every other prefix - including the lock - no
  matter what the flag says.
- Snapshot and project-registry reads are size-capped (a compressed-size ceiling and a
  decompressed-size ceiling), so a hostile-but-writable bucket cannot OOM the process
  with an oversized object or a gzip bomb.
- An artifact download whose origin differs from that of the server that resolved the
  collection is warned about (a visible signal in CI logs, printed even under
  `--quiet`) rather than blocked, so deployments that serve downloads from a separate
  content host or object storage still work. Origins - scheme, host and port - are
  compared rather than host names, because the origin is what decides whether your
  token and the server's TLS policy apply to a request: a download that keeps the host
  but drops to `http` or moves to another port reaches an endpoint that gets neither,
  so it warns too, even for a deployment that serves downloads from another port of
  its own host.
- A URL a Galaxy server supplied is refused outright when it embeds a credential in its
  userinfo (`https://user:pass@objects.example/a.tar.gz`), at both boundaries such a URL
  enters through: an artifact's download URL, and the metadata references a version walk
  follows (`versions_url`, `highest_version.href`). Left alone, that credential would
  replace the token you configured - Go's HTTP client turns URL userinfo into a Basic
  `Authorization` header before go-galaxy's own transport ever sees the request, and the
  transport does not overwrite a header that is already set. The refusal never prints the
  credential: the message names the refused URL with its userinfo and query cut out, and
  the run exits `5`. The same two cuts are applied to every line go-galaxy prints about
  such a URL - the lines it logs on the download path, the ones it logs while resolving
  metadata, and the source it names in a signature-verification failure - and to a URL it
  writes down for you to read, which is what the `download_url` and `version_url` recorded
  in `GALAXY.yml` are. A URL go-galaxy stores in order to fetch it again keeps its query,
  since cutting it would break the fetch that query authenticates: the cached API
  responses in the snapshot are that case. The request itself always carries the whole
  URL, for the same reason. A `versions_url` malformed enough that Go's URL parser rejects
  it is never judged by that guard, and it is never printed either: go-galaxy drops Go's
  own parse error, which would name the value whole, and reports instead that the metadata
  URL could not be built into a request, naming no part of it. A request that fails at the
  transport - a refused connection, a DNS failure, a TLS error - is re-rendered over the
  same two cuts, because Go's own report of it masks a password but leaves a query the
  server declared; the failure itself is preserved underneath, so nothing about retries
  or exit codes changes. Where the request had been redirected, that re-render names the
  URL go-galaxy asked for rather than the hop that failed, so a presigned redirect target
  never reaches the log at all - at the cost of the message no longer saying which hop in
  the chain was unreachable.
- A git repository named in `requirements.yml` is repository content, and everything
  about it is judged on that basis. Its URL may carry no credential; credentials are
  bound to a host through the environment and applied only when the scheme, host and
  port match (see [Git sources and
  credentials](servers-and-auth.md#git-sources-and-credentials)). Its path is held to a
  conservative alphabet and may not begin with `-`, because the remote's upload-pack
  receives it as an argument and go-git quotes but does not escape it. The remote is
  spoken to directly - no `git` binary, no credential helper, no `~/.ssh/config` - over
  this tool's own HTTP client, which carries no Galaxy token, relaxes no certificate
  check, and refuses a redirect that would move the session off the origin (net/http
  would otherwise forward a Basic `Authorization` header to a subdomain or across a
  downgrade to plaintext); an ssh host key must already be in known_hosts, with no
  first-use trust. What the remote can do to the process is bounded: the pack it ships
  is capped on disk (both transports converge there), the fetch is bounded by the same
  deadline as an artifact download, the body of an error response is cut at 64 KiB
  (go-git reads it whole to compose its error, and neither the stall watchdog nor the
  pack cap counts it), every tree entry name is validated before a path is built
  from it (no `..`, `.git`, separators, control runes or case-folded duplicates),
  blobs are capped at the archive's per-entry size, submodules are never fetched, and
  nothing is ever checked out, so no hook, `.gitattributes` or `.gitmodules` is
  interpreted. What remains bounded by go-git rather than by this tool is the inflated
  size of a single object during pack indexing, which is why the pack cap is far below
  the artifact cap. The artifact built from the tree passes the same manifest chain
  check and the same extractor a downloaded artifact does, and its identity is the
  `galaxy.yml` it was built from, compared against the collection being installed; a
  signature can vouch for none of it, so a verifying run reports a git collection as the
  vacuous pass it is.
- A role is held to the same boundaries, one level over. A `roles:` entry is repository
  content and is judged at the boundary like a collection entry: its Galaxy name, install
  name and version are held to their own alphabets at load, a git role's URL passes the
  same grammar (no credential, the conservative path alphabet, no `#subdir`), and the
  dependencies a fetched role's `meta/main.yml` and `meta/requirements.yml` declare pass
  that same grammar before anything is fetched for them, with a cap of 1000 roles per run
  on the walk and a 1 MiB cap on each meta file before it is decoded. Every role write
  goes through an `os.Root` established at `roles_path`, with the install name validated
  before it is joined, so a role named to escape the directory, or a role directory
  replaced by a symlink between runs, is refused by the kernel rather than by a check
  that raced it; `meta/.galaxy_install_info` is written through that same root. What a
  Galaxy server can do through its v1 role API is bounded too: a record's `github_user`
  and `github_repo` are held to the GitHub name alphabet and composed into an
  `https://github.com/<user>/<repo>` URL rather than copied from one the server sent
  (`download_url` is never read), its `github_branch` and every version name must pass
  the ref grammar, a `commit_sha` is kept only when it has the shape of one, and a
  pagination link is followed only within the server's own origin and for at most 20
  pages. A Galaxy role pin replayed from the snapshot, which anyone able to write the
  cache controls, is judged by the same rules before it can steer a fetch - its
  repository must be such a composed GitHub URL, its ref a qualified tag or branch,
  its version a valid role version - and a pin failing any of them fails the run
  rather than sending the role to another host. The v1 requests carry the token
  configured for that server and no other; the repository fetch that follows runs on
  the credential-free git client, so the Galaxy token never reaches github.com and a
  `GO_GALAXY_GIT_*` binding, matched by origin as for any git source, is the only
  credential that can. A role has no signature (the v1 API offers none) and no
  `sha256`; what stands in for attribution is the commit: a frozen install that has to
  rebuild a role fetches its pinned commit and refuses a repository that serves
  another. `cleanup` removes a role directory only under a roles path a project's
  registry record names, and only when the directory carries this tool's own extract
  marker - a role `ansible-galaxy` installed or somebody wrote by hand is never
  evidence for a delete, and a record written by a binary that predates roles carries
  no roles path and is never scanned.
- A `url` collection or role source is repository content, judged on the same basis as
  a git URL. The URL may carry no credential in its userinfo and no fragment, its path
  is held to a conservative alphabet and may carry no dot segment (a server resolves
  those while the credential match reads the path as written), and every line printed
  about it cuts userinfo and query exactly as artifact URLs are cut. One structural
  path form is admitted beyond plain segments: the path may embed an absolute http(s)
  URL - the caching-proxy shape `http://front/<upstream-url>`, where a front host reads
  the rest of the request path as the URL it fetches and caches - and the embedded URL
  must itself pass this same grammar in its canonical spelling, so the only empty
  segment such a path carries is the embedded scheme's own `//` separator; the
  credential-match side recognizes exactly that separator and nothing more, and fails
  safe to a credential-less request for any other spelling. The credential it
  may receive is never taken from the file and never inherited from a Galaxy server: a
  Bearer token is bound to an origin and an optional path prefix through the
  environment (`GO_GALAXY_URL_*`), attached only when the request's scheme, host and
  port equal the binding's and its path lies under the prefix, and that decision is
  made again on every redirect hop. A redirect into another origin - the ordinary
  GitHub release shape, a 302 into presigned object storage - therefore carries no
  Authorization header at all; go-galaxy does not rely on net/http's own stripping
  heuristic, which forwards the header to any subdomain of the host that set it. A hop
  that leaves https for plaintext http is refused outright. The Galaxy token never
  reaches a url source's host, even when the file names the Galaxy server's own
  origin, and a server's `validate_certs=false` relaxes nothing for a url download:
  the url client is built with no server configuration at all, so both are properties
  of its constructor rather than of a match a hostile file could steer. The artifact
  is pinned by the sha256 of the origin's own bytes, carried in the locator and the
  lockfile and enforced on every fresh download, cache hit, refetch and frozen
  install; its identity is its own MANIFEST.json, compared against the collection
  being installed on every refetch; and a url role's tarball passes the hardened
  extractor before anything reads its layout, then is rebuilt through the same role
  builder a git role is. A signature can vouch for none of it, so a verifying run
  reports a url collection as the vacuous pass it is, and a `signatures:` key on a url
  entry is refused at load.
- Pinned (`--frozen`) installs are already immune to a poisoned snapshot: for a
  lockfile-pinned collection, go-galaxy hashes the actually downloaded (or on-disk)
  bytes and compares them to the sha256 recorded in the in-repo lockfile, not to the
  cacheable metadata sha, so a poisoned download URL or sha causes the install to fail
  closed instead of installing attacker content. Use `--frozen` with a committed
  lockfile in CI as the robust mitigation against a compromised cache.
- **Signature verification (see [Signature verification](signatures.md#signature-verification))
  trusts the configured keyring a priori; it binds neither a key to a namespace nor
  ships any publisher's key.** No key is bound to any namespace: any key in the
  keyring may vouch for any collection. What IS bound is the collection identity
  inside the signed document itself - once a signature verifies, the manifest's
  declared namespace, name and version are checked against the collection this run
  actually resolved, so a key this run trusts cannot vouch for one collection while
  a different one gets installed under its name.
- **The default signature policy passes when nothing was gathered to check - but
  it is not silent about it.** A bare count (or bare `all`) is satisfied
  vacuously by an empty signature list; go-galaxy warns about it anyway, once per
  affected collection, naming the exact strict spelling (`+1`, `+2`, ...) that
  would make that same pass fatal. Bare `all` alone does not close the vacuous
  pass either - only the `+` marker does.
- **Tolerating a failure status is likewise not a silent defusal, with two
  precise exceptions.** When a tolerated status defuses every gathered
  signature and nothing is left verified, that is the same vacuous pass above
  and is warned the same way - but only under `all` or a bare `0`, the two
  spellings where that shape actually passes; under the default or any
  stricter bare count, a tolerated failure still leaves the required count
  unmet and the run fails instead. What genuinely is silent: the one-line
  announcement a verifying run prints names the keyring and the required count,
  never the tolerated-status list, and the six status codes this tool accepts
  but can never actually produce (it holds no secret key material and runs no
  `gpg` process) are tolerated with no warning of their own.
- **An already-installed collection is skipped without being re-verified - and
  the run reports it.** `install`'s ordinary skip logic for an already-settled
  collection runs ahead of signature verification, so the first run with
  `--keyring` turned on over an existing workspace verifies nothing; it says so
  once, on the result tier, naming how many collections were left unverified.
  `warm` carries no equivalent skip and re-verifies every collection it
  touches, cache hits included.
- **Nothing about a verification verdict is ever persisted - to the snapshot,
  the extract marker, the lockfile, or `GALAXY.yml` - which is the trust model
  working as intended rather than a gap.** This project's threat model already
  treats the cache as attacker-writable, so a cached "already verified" is
  exactly the kind of assertion signature verification exists to refuse to
  take on faith.
- **Revocation is only as fresh as the keyring file on disk.** There is no
  keyserver lookup and no network revocation check; a key revoked upstream
  after the keyring was last updated on disk still verifies.
- **A signature covers file content, not archive metadata.** The manifest
  chain check (see [Signature verification](signatures.md#signature-verification)) records
  a name-to-content-sha256 mapping; file modes, ownership, modification times,
  and the tar stream's own framing are all outside what a signature can be
  said to cover, as is any archive entry of a type the chain check does not
  record (only regular files, symlinks and hardlinks are).
- **A `requirements.yml` `signatures:` entry drives an outbound request this
  run would not otherwise make, and what one outcome discloses is broader than
  reachable-or-not.** No destination class is refused - loopback, link-local
  (including a cloud metadata service's own address), and unique-local
  addresses are all fetched exactly like any other - and a failed connection
  attempt's own message can include the resolved address and address family,
  the port, the connect error, and, on a TLS mismatch, the certificate's own
  list of valid names; redirects are followed too. This is a disclosed
  reachability oracle for whoever can edit the repository's
  `requirements.yml`, not a closed one, on the same grounds go-galaxy already
  accepts for a discovered `ansible.cfg`'s own `[galaxy] server`/`server_list`/
  `url` values.
- **A `file://` signature source reads a repository-chosen local path, and a
  failed read collapses into one message - but a read that succeeds can still
  disclose more than that message would suggest.** An absent path, an
  unreadable one, a non-regular file, and one too large to read all render
  identically; a file that opens and reads, though, is then handed to the same
  verification walk as any other signature, whose own failure messages can
  render the file's remaining byte count and other content-derived details.
  This is disclosed rather than bounded.
- **Two more residuals of signature verification are disclosed rather than
  closed.** A verifying run opens the artifact by path three times - to read
  its manifest, to check the manifest chain, and to extract it - so a local
  writer with access to the cache directory can swap the bytes between
  passes; that is the window the pin check, extraction and `cleanup` already
  carry for a live local writer, and closing it would mean holding one
  descriptor across every pass. And a `file://` signature source is read even
  after the per-collection signature budget has run out, because that read
  takes no context, so a blocking read on a hung mount is bounded by no
  deadline.
- **Converting a GnuPG keybox to a keyring this tool can read is the
  operator's own step**, not something go-galaxy does automatically - a
  `.kbx` file is refused by name, with the export command to run instead.
- **Under `--offline` with a cold API cache, a verifying run can learn nothing
  about whether a collection is signed at all - and it says so distinctly.** A
  collection installed this way is not silently reported as unsigned; the
  vacuous-pass warning it prints names the metadata as unavailable rather than
  reading like "no signatures found".
- **`--frozen` and `--no-deps` normally skip a per-collection metadata request
  for an already-cached artifact; a verifying run cannot.** A server's own
  signatures live in that same version-metadata document, so turning on
  `--keyring` under either flag reintroduces a metadata request per collection
  that would otherwise have been skipped.
- **Signature verification covers the artifact a collection resolved to,
  never the dependency graph that led there.** A collection's dependencies are
  always taken from the Galaxy API's own dependency map for the resolved
  version, never from the signed manifest's own `dependencies` field.
- **A cache must not be shared between principals holding different Galaxy
  credentials.** A local cache directory or an S3 bucket is not scoped to a
  credential: cached API responses, resolved versions, dependency graphs, and
  artifacts are keyed by server, not by which token fetched them, so an entry a
  privileged run fetched from a private server is served as-is to a later run
  against the same server with no token at all. Keying by a token fingerprint
  instead was considered and rejected: it would put a credential-linked
  identifier into a shared, less-trusted store, which is exactly the trust
  boundary above this design keeps clean. Give each distinct credential its own
  cache directory or S3 prefix/bucket.
- **No token ever reaches persisted state.** Neither a token nor any value
  derived from one - not even a hash - is ever written to the local snapshot,
  the S3 snapshot, the project registry, the lockfile, the metrics file, or
  GALAXY.yml. A configured token is rendered as a fixed redacted placeholder by
  every serialization path (`fmt`, JSON, YAML), and its plaintext is reachable
  through exactly one call site in the whole program, immediately before it is
  attached to an outgoing request.
- **Operator-facing output is sanitized before it reaches stdout or stderr.**
  Text this program did not generate itself - a Galaxy server's HTTP reason
  phrase, an S3 error body, a lockfile entry's `name`, a manifest, a
  filesystem path - can carry ANSI escape sequences or other control bytes a
  terminal or log processor would act on; every such byte is replaced with
  `U+FFFD` (newline and tab are kept) before printing. `outdated`'s own
  report is sanitized on the identical boundary as every other command's
  output: a hostile server's HTTP reason phrase, printed on its `Lookup
  failed:` line, and a lockfile entry's `name`, printed on every line that
  names it, are both neutralized before printing rather than reaching the
  terminal raw.

## Boundaries in detail

The sections below go part by part through what each boundary refuses and
which property of the code it rests on - the constraints a change to that part
has to keep.

### Credentials and the token pairing rule

A credential's plaintext is revealed at exactly one point per kind, and each
hands it straight to the client that puts it on the wire: `serverAuths` for a
Galaxy token (into `fetch.ServerAuth`, since `fetch` sits below `config` and
cannot hold a `Secret`), `gitCredentials` for a git password, ssh key and
passphrase (into a `gitsource.Credential`, which nothing may print, log or
persist), `urlBindings` for a url token, and the S3 client for its secret key
and session token. A new kind of credential gets one such point under the same
rule.

A url token's binding is matched to a request by byte equality of two origin
renderings - `urlsource.Prefix.Origin` for the binding, `helpers.Origin` for the
request, both `scheme://host:port` with the default port filled in and IPv6
brackets stripped - so changing either renderer alone silently stops the token
from being attached.

A Galaxy token is attached by the same kind of comparison: only when the
request's `helpers.Origin` is byte for byte a token-bearing server's. There is
no host, prefix or suffix match, so another port or the `http` twin of an
`https` server gets no token, and a download URL taken from a poisoned snapshot
cannot draw it to a look-alike host. An `Authorization` header already set is
never overwritten, and the transport prints itself as a count of origins, never
an origin or a token.

The pairing rule described under [--token](servers-and-auth.md#--token) rests
on provenance `config` records for each server as it builds it: whether its URL
and its token came from the ansible.cfg file rather than from the environment
or a flag, and whether the file, not the environment, disabled its certificate
verification - a server whose certificates are verified never reads as relaxed
by the file. `applyTokenFlag` clears the token's file provenance when it
installs an operator token, so the pairing check has to run after it, or it
would exempt that token as the file's own. The bare `[galaxy]` server can only
commit the destination offense, since `[galaxy]` has no `validate_certs` key.

The rule protects only a token you supplied, and two shapes pass it by design.
A file that disables verification for an anonymous run, or pairs its own
`validate_certs` with its own token, draws only the warning described under
[TLS: validate_certs](servers-and-auth.md#tls-validate_certs). And a URL or TLS
policy you supplied, paired with a token the file supplied, is accepted,
because the credential sent is not yours. The accepted cost: a collection your
account could see and the file's token cannot resolves as a 404 from that
server, so under a multi-entry `server_list` the walk moves on to the next
server without saying why.

In ansible.cfg, a line that opens with `[` but is neither a section header nor
a key line - a header never closed, or one whose `]` an inline `;` comment cut
off, such as `[galaxy_server.dev ;scratch]` - closes the current section, so
the keys below it belong to none. Python's `configparser` refuses such a file
outright, so there is no ansible behavior to match. Keeping the previous
section instead would file a `url` written for `[galaxy_server.dev]` under the
`[galaxy_server.prod]` above it, and prod's token, which the pairing rule lets
through as the file's own, would go to the dev host. A line `configparser`
reads as a key line (`[note = x`) keeps its section; the rest of the parsing
rules are under [What go-galaxy reads](configuration.md#what-go-galaxy-reads).

### Redirects

net/http builds every redirect hop as a new request and runs it through the
transport again, so each credential decision above is made anew per hop: a
redirect into another origin carries no Galaxy token. A url token is decided
per hop in both directions - a hop out of a bound origin loses the Bearer
header, and a hop into one gains that binding's token, however the request got
there. The url transport also repeats the grammar's check for dot and empty
path segments (`%2e` folded to a dot) on every request it is handed, not only
one that passed the requirements boundary: a path carrying one, or a spelling
it does not recognize, is sent with no credential rather than refused.

Every client `internal/galaxy/fetch` builds, the offline one included, deletes
the `Referer` header net/http composes for a hop. net/http strips userinfo
from it but keeps the query, so a presigned URL redirected to another origin
would hand that origin its signature. Installing that hook replaces net/http's
default redirect check, so the hook re-imposes the ceiling of 10 hops, in
net/http's own `stopped after 10 redirects` wording so the failure classifies
as it always did.

A url download that leaves `https` for `http` is refused even when no
credential is bound, because the first, unpinned fetch takes the sha256 pin
from those bytes. The floor is the scheme of the first request, so a
requirement written as `http` may redirect over `http`.

Each of those clients honors `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`, and
the credential-free ones promise nothing about the proxy itself: a proxy URL
carrying userinfo makes net/http send its own `Proxy-Authorization` header to
that proxy, whichever client made the request.

### URLs a Galaxy server supplies

The userinfo refusal above sits on two boundaries that otherwise judge
differently, on purpose.

A download URL is checked once per artifact acquisition, before any request is
built, and is fetched only when it is an absolute `http` or `https` URL naming a
host. A `file:` URL - the shape a poisoned snapshot would use to aim a fetch at
the local filesystem - any other scheme, a relative reference and a value Go
cannot parse are refused as a metadata failure. Scheme and host are
judged before userinfo, and neither refusal renders the raw value or Go's own
parse error, which would print it whole.

A metadata reference - a root document's `versions_url` or its
`highest_version.href`, fresh or replayed from the snapshot - becomes a request
URL through one function, `normalizeVersionsURL`, whose userinfo refusal judges
the resolved result: a relative reference resolved against a
credential-carrying source is refused like an absolute one carrying its own.
The refusal runs after the server walk and must not move inside it, since it has
no honest HTTP status: routed as a 404 it would silently advance to the next
server, and as a 401 or 403 it would abort naming a credential problem that
does not exist. It keeps no scheme allow-list, because the fallbacks that
resolve a reference can yield shapes nobody has surveyed and a guessed list
could refuse a real deployment's metadata; another scheme fails in Go's HTTP
client instead.

The credential cut applied to `GALAXY.yml`'s `download_url` and `version_url` is
not redundant with those refusals. The download-URL check runs only when an
artifact is downloaded, so a cache hit served with its metadata in hand records
a download URL no check has seen, and `version_url` is judged by no fetch-side
check at all. The `signatures` list, by contrast, is only reshaped to the keys
ansible's schema admits; every string it keeps is written exactly as the server
sent it.

A failed request is re-rendered with its URL cut (`helpers.CutTransportURL`),
but the wrapped cause is printed as it is, so no `http.RoundTripper` this
program installs may put its request URL uncut into an error it returns - the
offline transport cuts its own. The original error stays reachable underneath,
which is why every classifier, retries and exit codes alike, has to use
`errors.Is` or `errors.As` rather than a type assertion.

A query on a Galaxy server base or on a collection's `source:` - a capability
such as `?tok=...` - is neither refused nor cut, since configuration and
requirements parsing check userinfo only. That is harmless only by
construction: the API root candidates are built by string concatenation, so the
appended API path lands inside the query and no request ever names an API root.
Building them with `net/url` instead would start resolving such a base and write
its query uncut into the snapshot, the committed lockfile and a printed warning,
so a change there has to refuse or cut the query first.

### Git and url sources

A git repository path may carry no empty or dot segment, a percent-encoded dot
included: the remote resolves `/org/../other/repo.git` to `/other/repo.git`,
while a `GO_GALAXY_GIT_*` binding is matched against the path as written, so a
dot segment would let a requirements file spend a credential bound to one
prefix on any repository of the host. The url grammar holds the same rule,
bar the embedded upstream URL's `//` described above, and one path check
serves both a url requirement and a `GO_GALAXY_URL_*` prefix, so the request
and the credential match can never read one path differently. A
git binding and a repository path are compared without their leading slash, so
a binding `ssh://host/org` also covers the scp-like `git@host:org/repo`; the
ssh login is never part of the match.

A url locator always carries its `#`: `url+<url>#` before resolution and
`url+<url>#sha256:<hex>` after, the one string the artifact key, the installed
record, the snapshot and the lockfile carry. It parses left to right only
because a url source may carry no fragment, and a locator read back is
accepted only in its canonical spelling - a URL that round-trips unchanged and
a lowercase 64-hex pin - so a hand-edited record is refused rather than
reinterpreted. A Galaxy entry whose `source:` is a git pointer or a git or url
locator is refused at load: every later consumer dispatches on the locator
prefix alone, so such a source would be fetched without its URL ever passing
either grammar. `type: git` and `type: url` are the only spellings of those
sources.

### Loading requirements.yml and the lockfile

A lockfile is repository content just as `requirements.yml` is, so an entry
whose `source` embeds userinfo is refused at load, for a collection entry and a
Galaxy role's alike (git and url sources refuse one by their own grammars).
The source would otherwise flow unchanged into metadata requests, warnings, the
snapshot and `GALAXY.yml`, and net/http turns URL userinfo into a Basic
`Authorization` header the token transport never overwrites, so a committed
credential would displace your token for that host. The refusal exits `6`,
names the entry and never prints the source. A bare `server_list` id such as
`internal`, which has no scheme or host, is left alone, as it is in
`requirements.yml`.

Inside `internal/galaxy/requirements`, validation order is part of the same
boundary. The refusal of an entry with no name renders the whole raw entry, so
the userinfo check on `source:` and the check of the `signatures:` sources run
before it; a git or url entry refuses a `signatures:` key before anything
echoes the entry, and a `signatures:` value of the wrong shape is named by its
Go type, never its content.

galaxy.toml is repository content on the same terms and is judged by the same
validators in the same order: the TOML front end only reshapes the decoded
document into the tree `parseRaw` judges - a dependency string into the
`name`/`version` mapping the YAML entry would be - and never builds a
requirement of its own, so no rule above has a second implementation to drift
from the first. What the format adds is held to the same discipline. A TOML
syntax error is rendered as its line and last key only, because the lexer's
own messages can echo a string body or a bare token from the file into the
output; the schema is closed, so an unknown top-level table, an unknown
`[project]` key and an unknown key on a collection or role inline table are
refused by name rather than ignored or warned about; a scalar of the wrong
type is named by its key and Go type, never its value; and a version
constraint is checked with `semver` at load and rendered without the library's
own message. Discovery only `Stat`s, and admits only a regular file: a
`./galaxy.toml` that is a directory or a fifo is skipped with a warning rather
than opened, since the open happens under the exclusive cache lock, where a
fifo would block the run and, on S3, every other runner sharing the bucket.
The rule that drops `./ansible.cfg` in a world-writable directory (see
[Configuration](configuration.md#ansiblecfg)) is deliberately not mirrored
for either requirements file: the requirements file is the input being
installed, not a setting that redirects where an install lands or which cache
it deletes beneath, `./requirements.yml` was never defended that way either,
and a `0777` CI workspace has to keep working.

### Archive extraction

`internal/galaxy/archive` owns extraction and refuses each unsafe shape under
its own sentinel: an entry path that is empty, absolute or escapes the
destination; an existing symlink anywhere along an entry's path or a hardlink
target's path; a symlink target that is empty, absolute, or resolves outside the
destination, to its root or to itself; a regular-file entry for a path an
earlier entry already created, as a file or a directory, which is refused
rather than resolved as last-wins, since a collision is undetectable once
extracted; and anything past the entry-count, per-entry, total and
decompressed-size caps. A gzip member that produces no bytes is refused one
layer down, in `internal/gzipstream`. An entry of a type other than directory,
regular file, symlink or hardlink is skipped rather than refused. The package
resolves no path through `os.Root`: its caller hands it a destination it has
already contained (see [Verify, extract,
record](architecture.md#verify-extract-record)). The duplicate refusal rests on
the first copy having been created read-only, so a process allowed to write a
read-only file anyway - root, through `CAP_DAC_OVERRIDE` - reopens it, and the
last regular-file entry for that file wins instead.

Three bounds split the work, and none subsumes another. Every header
`tar.Reader.Next` returns counts against `helpers.ArchiveMaxEntryCount`,
whatever its type, so a tarbomb of zero-byte directories or hardlinks is
refused; this bounds headers, not inodes, since an entry `a/b/c/f` can create
four. Every counted header's declared size is charged against
`ArchiveMaxEntrySize` and `ArchiveMaxTotalSize` before the entry is dispatched
on its type, so an entry that writes nothing - a directory declaring a size, a
name that normalizes away, a skipped type - is still charged, and a negative
size, which archive/tar passes through intact from a GNU base-256 field on a
header-only entry, is refused rather than allowed to lower the running sum.
Neither is the guarantee, because a declared size does not bound the bytes
read: a sparse entry (old-GNU `S`, or PAX) declares its logical size while
archive/tar reads its physical body, and `x`, `L` and `K` meta headers are
consumed inside `Next` without ever being returned. The guarantee is
`ArchiveMaxDecompressedSize`, enforced around the gzip stream rather than in
the entry loop, so it sees every byte archive/tar consumes, tar framing and
padding included. It leaves no headroom for that framing on purpose: headroom
sized from the entry count would be an allowance for chains of meta headers,
which `Next` never returns and the count never sees. Below that ceiling
amplification is accepted: a few sparse entries each declaring one byte can
carry a stream just under the cap. Ahead of all three, one download may write
at most `ArtifactMaxDownloadSize` compressed bytes (equal to the total cap) to
its temp file, whether or not extraction runs.

The reader that enforces it (`decompressedLimitReader`, shared with
`ProbeTarGz`) keeps properties a change must not lose. The read that crosses the
cap returns zero bytes with the error, never the bytes it just read, because
`io.ReadAtLeast` (archive/tar's header read) and `io.CopyN` (its skip of an
unread body, and the file copy) drop an error paired with a satisfied request,
and returning the bytes makes the refusal vanish. The error is sticky, and the
overrun is clamped to exactly one byte. Neither it nor the cancellation reader
beside it may implement `io.Seeker`, or archive/tar would skip a body by
seeking, past both the count and the cancellation check. It does not reuse
`helpers.NewSizeLimitedReader`, whose sentinel classifies as a network failure
(exit `4`); a decompression bomb exits `5`, as an archive failure. The three
readers - this one, `helpers.NewSizeLimitedReader` and the manifest scans'
`limitReader` - keep exactly the same shape, though, and a change to one has
to be made to all of them.

Cancellation is observed on both sides of the decompressor: on every
decompressed read, so one huge entry is interruptible mid-body, and in
`internal/gzipstream` on every compressed read, so a member spending input
without producing output still stops. A fired check surfaces the context's own
error unwrapped, so an interrupt still exits `130`, and a probe that was
interrupted or ran out of time is not reported as a bad artifact.

`internal/gzipstream` is the one place a gzip reader opens over bytes this
program did not produce: an artifact, and the S3 backend's state objects.
pgzip is never read directly, because its `Read` finishes a member by calling
itself and Go eliminates no tail call, so a stream of members that decompress
to nothing would recurse once per member until the goroutine stack overflows -
a fatal error no `recover` catches, chosen by the archive's own bytes. The
wrapper walks members in a loop and refuses the first one that produces no
bytes (`ErrEmptyGzipMember`), which no legitimate input carries: a tar stream is
at least its 1024-byte end marker, and a snapshot never marshals to nothing.
Non-empty members are bounded only by the compressed-size caps and each read's
deadline. The one gzip reader left outside on purpose is net/http's
transparent `Content-Encoding: gzip` decoding on the fetch clients, whose
`compress/gzip` walks members in a loop, so a member flood there costs only
time under the request's own deadline.

Within one extraction, a path component already proven a real directory is not
checked again (`verifiedDirs`). That is sound only because extraction never
replaces an existing path with another kind of object - `os.Symlink` and
`os.Link` fail with `EEXIST`, opening a directory as a file or creating a
directory over a file fails, and every such failure aborts the extraction; a
change that removed and recreated, or overwrote, an existing path would reopen
the symlink-component attack through the memo. The memo records only
directories, so a symlink an earlier entry created is still `Lstat`ed when a
later one names a path through it, and an `Lstat` failure other than not-exist
fails the extraction rather than reading as an absent component. A directory
`os.MkdirAll` found already present may be memoized only because every
component of the entry's path was `Lstat`ed first (`MkdirAll`'s own `Stat`
follows symlinks), so a new caller of `ensureDir` must run that check before
it.

### Reading a manifest

`internal/galaxy/manifest` writes nothing, not even a temporary file: the
documents it reads decide whether an install proceeds, so the code reading
them is kept unable to change what is installed, the same separation
`internal/galaxy/signature` keeps around its verdicts. It resolves no path
through `os.Root` because every path it is handed names an artifact file -
downloaded, built or taken from the cache - never one walked out of an
archive; making it open an
archive-derived path, or write anything, breaks that boundary.

`ReadFromTarGz` returns only the artifact's own manifest: the first
regular-file entry whose name, cleaned with `path` rather than `filepath` (so a
backslash is an ordinary character on every OS), is exactly `MANIFEST.json`. A
nested `vendored/x/MANIFEST.json` is walked past even when it comes first,
since a base-name match would return a decoy. Every entry the walk passes is
held to the per-entry cap by its declared size, since the walk reads past it,
and its name to `ArchiveMaxEntryNameLen` (1024 bytes) before any refusal can
quote it. Crossing the 64 MiB scan budget, a stream ending with no match and a
zero-byte manifest are all `ErrManifestNotFound`, so a manifest returned always
holds at least one byte. A decompressor that failed to open because the run
was canceled reports the cancellation, not a bad artifact.

The chain check bounds its pass exactly as extraction does: the entry-count
cap, every declared size charged before the entry is dispatched on its type
(the two passes share the constants, not the code), and a decompressed cap of
exactly `ArchiveMaxDecompressedSize` - a lower one would refuse artifacts the
extractor accepts, since this pass reads every body to hash it, and a higher
one would accept more. Unlike the extractor, whose memo holds only directories,
it keeps the name of every file and link until the stream ends, since
`FILES.json` may sit anywhere; archive/tar alone accepts GNU long names up to
1 MiB, so every name and link target is capped at `ArchiveMaxEntryNameLen`, on
each header before any rule renders a name, or about 4 GiB of names could be
held under the decompressed ceiling.

`FILES.json` is the one entry held whole, so a header declaring more than
`FilesManifestMaxBytes` (32 MiB) for it is refused before its body is
allocated; it is then decoded row by row, at most `ArchiveMaxEntryCount` rows,
since decoding a 32 MiB listing of empty rows at once costs over a GiB of
heap. Every string of a listing row and of `MANIFEST.json`'s
`file_manifest_file` pointer is capped at the same 1024 bytes, and a refusal
reports the field's length, never its value. That cap bounds rendering only:
the JSON decode has already run, and `encoding/json` turns each invalid UTF-8
byte into a three-byte `U+FFFD`, so a manifest at the 64 MiB scan budget can
decode into a string of about 192 MiB first. That residual is accepted.

### Signatures and OpenPGP framing

`internal/galaxy/signature` only reads: the keyring the operator chose, and a
`file://` source the repository chose, opened read-only, so the install
pipeline's rooted writes stay the only writer under `ansible_collections`. Its
`Fetcher` builds its own client from `fetch.NewUnauthenticated` and accepts
none from a caller, so no caller can hand a repository-authored source a client
carrying a token or a relaxed TLS policy. One consequence: a source on a host
with a private CA fails certificate verification even where a configured
server on that origin has `validate_certs=false`, and its CA has to be trusted
system-wide (see [TLS: validate_certs](servers-and-auth.md#tls-validate_certs)).
`FetchRequirementSource` accepts the `file` scheme only because its value comes
from a requirements file. A signature URI supplied by a server's metadata or
replayed from the snapshot, which a principal with no commit access can reach,
needs a sibling method that refuses `file` outright, never this one.

Everything the fetcher reports is built from one display form, computed before
`url.Parse` runs, so no message prints a source's userinfo, query or fragment,
even for a value the parser rejects; the query is still sent. Under
`--offline` an `http(s)` source is refused before a request is composed, since
the offline transport's own message would render the query. A `file://` source
is opened with `O_NONBLOCK` and judged a regular file on the opened
descriptor, never by a stat of the path first: a blocking open of a planted
FIFO would outwait every deadline, and a path stat would race the open. Every
failure collapses into one error that wraps no OS error, so no caller can
branch on `fs.ErrNotExist`; the collapsed outcomes still differ in wall-clock
time by microseconds, a side channel accepted under the phase's network-latency
budget.

go-crypto's packet parser sizes buffers from lengths the stream declares before
it discovers the stream is short: ten bytes declaring a v6 signature with a
hashed subpacket area of `0xffffffff` bytes allocate 4 GiB. Every packet
stream - a detached signature blob and every keyring, binary or armored -
therefore passes the package's own framing walk before any go-crypto parse call
sees it. Armor is decoded by the package itself, never through go-crypto's
armored entry points, which hand the decoded stream straight to the parser,
and the walk runs on the decoded bytes, since a check of the raw file would let
every armored attack through. A new path that reaches
`openpgp.CheckDetachedSignature` or `openpgp.ReadKeyRing` without the walk
reopens the amplification, and a refused packet refuses the whole file, never
a partial keyring or a partially checked blob.

Per packet, the walk first refuses a framing with no findable end (a new-format
partial length or an old-format indeterminate one, whatever tag carries it,
since the tag is input-chosen) or a header it cannot read, a leading one
included, since go-crypto scans past unreadable bytes ahead of key material;
then a secret key or subkey packet; then a tag outside the input's allow-list,
and more packets than its ceiling. It then checks that the declared body is
present and walks a v4, v5 or v6 signature's hashed and unhashed subpacket
areas, recursively through embedded signatures to a depth of 4, refusing a
length past its bytes and a subpacket with a type octet and no body, which
panics go-crypto's parser. That walk has to precede the parse, since it is what
bounds the parser's allocations. Last, go-crypto's own packet reader runs over
exactly that packet, and any byte it leaves unread refuses the file: a short
parse would leave the shared reader inside the body and cut every later packet
at an input-chosen offset. The parse error itself is ignored, so a packet
go-crypto merely skips, such as one with an unknown key algorithm, is not
refused. Secret key packets are refused at the header rather than parsed also
for cost: parsing one ends in RSA key validation that grows faster than
linearly with the modulus, about 12 seconds for one 16 KiB packet, which no
size or packet ceiling bounds.

The armor decoder reads through a 100-byte buffer and grows a long header line
chunk by chunk, so an N-byte line costs about N*N/200 bytes of allocation. The
header section behind every armor opening line, not only the first, is
therefore held to 4096 bytes, which keeps the cost linear in the input; every
one is judged because the decoder abandons a block at a header line with no
colon and restarts at the next opening line. A block body needs no bound, as
the decoder refuses a body line over 96 bytes. The gate fails closed: a line
shaped like an opening line (`-----BEGIN` and more) that is too long for the
decoder to read, followed by more than 4096 bytes with no blank line, refuses a
blob as `ERRSIG` even when a valid block follows, since narrowing that would
tie the refusal to the decoder's unexported buffer size, which a dependency
bump can move. The residual is disclosed: a 1 MiB signature
blob of maximal sections costs about 25 MB of allocation, so about 1.6 GB per
collection across its 64 blobs, spent one blob at a time rather than held at
once, while a keyring pays for one section, since its first block that is not
key material refuses the file.

The keyring loader never reads a keyring short in silence: a key that dropped
out of the trust set would make a correctly signed artifact report as signed by
nobody trusted, with nothing naming the cause, so every path that could lose
keys is a refusal. It cuts the file into armor blocks itself rather than
calling the decoder in a loop, which buffers and can consume input past a
block's end line, and it cuts only at opening lines that begin a line; one
found mid-line refuses the file rather than widening the cut, which could merge
two blocks. The packet ceiling belongs to the whole file, each block charged
before it is decoded, so adding blocks buys no extra parsing. Its size refusal
does not use `helpers.NewSizeLimitedReader`, whose sentinel classifies as a
transport failure rather than a configuration error. A signature blob is
treated differently on purpose: only its first armor block is decoded and the
bytes after it are granted nothing, since a blob is one signature source, while
every block of a keyring is material the operator meant to trust.

### The collections tree and the cache directory

Every install-side read and write in the collections tree goes through one
`os.Root` per run, opened at the download path before anything is resolved. It
is never opened at `ansible_collections` beneath it: `os.OpenRoot` follows a
symlink at the path it opens, so rooting one level lower would adopt wherever a
planted `ansible_collections` link points. An `ansible_collections` leading out
of the download path therefore fails the whole run once with
`helpers.ErrCollectionsPathEscape` (exit `5`), a namespace or name directory
leading out is refused where it would be written, and neither is ever written or
deleted through, or taken as evidence that a collection is already installed.
Two symlink shapes have to keep working, because a collections path is often a
cache mount or a workspace alias in CI: the download path itself may be a
symlink, and `ansible_collections` may be one resolving inside the download
path - through a relative target only, since `os.Root` refuses every absolute
target, even one leading back inside. `cleanup` roots its removals at the same
place for the same reason.

A collection's namespace, name and version are each held to
`helpers.IsPathElement` before any path is joined from them: when the resolved
set is folded into the install plan, again in `newInstallTarget`, and in
`cleanup`'s removal - the same predicate on the same three components, so
`cleanup` can always remove a `.info` directory install created.
`helpers.SplitFQDN` checks only the two-part shape, and a caller's check never
makes a callee's guard redundant. The composed `<ns>.<name>-<version>.info`
element cannot be validated after the join instead, because `path.Join` fuses a
version starting with `..` into an element the next `..` cancels, and every
further `..` climbs a real directory. Where a version is judged by
`helpers.IsExactVersion` alone, that is enough only because every value it
accepts is also a path element (see [Layering](architecture.md#layering)).

The local artifact store, unlike the extracted store and `roles_path`, writes
through no `os.Root`; its safety rests on key shape instead. Every key is built
by `helpers.ArtifactKey` - a server fingerprint, a dot, and the filename through
`url.QueryEscape`, which escapes every separator - or is the legacy escaped
filename `cleanup` sweeps, so every artifact path is the cache directory joined
with exactly one element. The store trusts its callers for this and checks only
that a key is not empty, so a key that could carry a separator or a dot segment
would void the guarantee. With one element, the entry and its `.sha256`
sidecar are the only names a writer to the cache directory can replace with a
symlink, and a commit or a delete acts on the link, never on its target: a
commit renames the entry into place and writes the sidecar to a temp file
renamed onto its name, and a delete unlinks both. The two sweeps of that flat
directory, `--clear-cache` and the removal of download temps a killed run left
behind (a sidecar's temp carries the same prefix), keep to the rule: each
removes one top-level name with a plain `os.Remove` and skips directories, so a
symlink planted at a sweepable name is unlinked and its target survives. A
change that nests these paths, writes an entry or a sidecar in place, or opens,
truncates or follows one before deleting it, loses that property and needs a
containment root.

A sha256 digest has one accepted shape, 64 lowercase hex characters
(`helpers.IsSHA256Hex`), and an uppercase one never reaches the cache. A
server's advertised digest and an S3 object's recorded one are compared with
the digest this process computed by exact equality, so a case-only difference
is a mismatch - a failed download, or on S3 an eviction and a refetch - and a
local sidecar that fails the shape is ignored. Making either comparison
case-insensitive would let the two backends disagree on what a valid cached
digest looks like, and widening the predicate would reopen the traversal the
extract marker's path relies on it to close (see [The extract-done
marker](architecture.md#the-extract-done-marker)).

The lockfile and the metrics report, the files written for you to read, go
through `helpers.WriteFileAtomic`: a temp file in the target's own directory,
synced and renamed onto the path, so a symlink there is replaced rather than
followed, with mode `0644` set regardless of umask so a CI consumer can read
it. A cache-internal file that must stay private cannot use it; the project
registry keeps the `0600` of `os.CreateTemp` by doing its own temp-and-rename.

### Reading an installed tree: cleanup and outdated

`cleanup` treats every recorded collections tree as untrusted, and it computes
reachability over every registered project before it deletes anything, so
whether one project's requirement keeps another project's copy alive never
depends on the order projects are visited in. What it does is described under
[cleanup options](cli.md#cleanup-options); what it relies on is below.

A project's workspace is rooted at its collections path, never at
`ansible_collections`, and `ansible_collections` is probed through that root.
The candidates are tried in order - the recorded collections path, then
`<project>/.collections` and `<project>/collections` - and a probe that fails
with anything but not-exist (an escaping symlink, a loop) skips the whole
project with a warning instead of falling through to a later candidate, which
would silently aim `cleanup` at a directory nobody configured. The escape is
caught only at that probe; a swap between the probe and the directory read
aborts the run as a scan failure.

The scan reads exactly `ansible_collections/<ns>/<name>/MANIFEST.json` and
indexes only entries that are real directories, since a symlinked namespace
would be an intermediate component the rooted removal resolves, deleting inside
its target. A collection's namespace and name come from the two directories
walked, never from its manifest, so no manifest can aim a record, an artifact
purge or a delete at another collection; only the version comes from the
manifest, since nothing else on disk records it. All three must pass
`helpers.IsPathElement` - a failure is warned about and skipped at scan, and
aborts at removal - which also stops a newline from forging a line in the
report. A lying version can at worst mistarget the best-effort `.info` removal
or an artifact slot a refetch heals; it never joins a directory path.

Removal re-opens its own `os.Root` at the collections path and re-checks the
identity, so a namespace or `ansible_collections` swapped for an escaping
symlink between scan and removal is refused, even though the lexical
within-directory check still passes. Two residuals are accepted, since
exploiting either already takes write access to the collections path: a symlink
planted deeper inside it is not defended, and a writer who replaces the
collections path itself between the scan and the removal can redirect the
removal into a tree of its choosing.

`cleanup` opens no file before its type is known. A recorded requirements file is
`Stat`ed and refused unless it is a regular file, which aborts the run before
anything is deleted: opening a fifo blocks until a writer appears, while
`cleanup` holds the exclusive cache lock with no deadline of its own - and on S3
the lock heartbeat keeps renewing, so one planted pipe would lock every runner
sharing the bucket out indefinitely. `Stat` rather than `Lstat` keeps a
symlinked `requirements.yml` or `galaxy.toml` legal, and a dangling one reads
as a stale registry entry. A `MANIFEST.json` is gated the same way but with
`Lstat`, so a symlinked
one is skipped too, because `os.Root` follows a relative link inside the root.

`outdated`'s installed-tree fallback keeps the same discipline. Every file it
reads there - `GALAXY.yml`, `go-galaxy.yml`, `MANIFEST.json`, a role's
`meta/.galaxy_install_info` - is `Lstat`ed through the root and read only when
it is a regular file, since `os.Root` bounds where a path may lead but not what
sits at its end. A sidecar contributes a lookup only when its namespace and
name pass the alphabet of the install's own kind (ansible's wider mixed-case
rule for a url install, the Galaxy alphabet otherwise), its version is exact,
and `<ns>.<name>-<version>.info` recomposed from its own fields names the
directory it was found in. That last check stops a sidecar planted under one
collection from attributing a version to another; the first two keep a hostile
value out of the Galaxy request URL, and narrowing the url alphabet would
silently drop from the report a mixed-case collection `install` accepted.

### The S3 endpoint

The S3 client refuses every redirect its endpoint answers with, a same-host
upgrade from `http` to `https` included, and never retries the refusal: the run
exits `2` (`cache backend cannot be used as configured`) with a hint to set
`--s3-endpoint` to the redirect's origin. A redirected SigV4 request could not
verify anyway, since the signature covers the host and the path, and following
one would only add exposure: Go's HTTP client strips `Authorization` on a
cross-host hop but not the `X-Amz-*` headers, the session token among them; a
307 or 308 replays a PUT body (the snapshot, the project registry, the lock
object); and an https-to-http hop carries the signed headers in cleartext. The
upgrade is refused because the request that drew it already went out in the
clear. The refusal is installed on the S3 client's own copy of the shared HTTP
client, so Galaxy redirects are still followed.

`--clear-cache` on S3 deletes by prefix, one Multi-Object Delete per listing
page of at most 1000 keys, so memory stays bounded to a page. The request body
is marshaled with `encoding/xml`, never templated, because the keys come from
the bucket listing and are untrusted. A `200` is not success on its own: S3
reports per-key failures inside a `200` body, so the body is parsed, and a key
that failed fails the run without a retry. Beside the snapshot and registry
caps above, both XML responses - a listing page and a delete result - are read
under a 16 MiB cap per response, so a hostile or broken endpoint cannot exhaust
memory with one; an overrun names which response it was and is never retried,
since an endpoint that overran once will again.

### Printed output

`safeout.Clean` replaces rather than deletes, since a deletion can mislead on
its own (`good\rbad` would read as `goodbad`). It replaces every C0 control but
newline and tab, DEL, every C1 control whether validly encoded or a raw
0x80-0x9F byte, `U+2028` and `U+2029`, and every byte that is not valid UTF-8,
so its output is always valid UTF-8. The two line terminators are not controls,
but Unicode-aware consumers such as Python's `str.splitlines()` in CI tooling
split lines on them. The bidi and format controls (`U+202A`-`U+202E`,
`U+2066`-`U+2069`) are kept on purpose: they cannot move the cursor, clear a
line, set a title or emit a hyperlink, and a real defense against visual
reordering would also have to cover homoglyphs and combining marks, so a
partial one would promise a guarantee this tool does not give.

Newline is kept because joined errors are separated by it, and tab only moves
the cursor forward; a hostile message can add plain lines but never overwrite
one already printed, since `\r` does not survive. Sanitizing output therefore
cannot stop an untrusted value from forging a whole extra line. That is stopped
where the value enters instead:

- `helpers.IsPathElement` refuses every rune `safeout.IsUnsafeRune` reports,
  newline and tab included, which makes it a printing guarantee: a value built
  only from validated components - a namespace, a name, a version - may be
  printed with a bare `%s`. It accepts invalid UTF-8, which prints as a visible
  `U+FFFD` and cannot forge a line; refusing it would stop `cleanup` from
  reclaiming a non-UTF-8 name a filesystem actually holds. Loosening its rune
  check would let forged lines back in at every such call site.
- A lockfile entry's name is held by `lockfile.Load` to the collection-name
  alphabet (or the url-collection one, for a url entry), which refuses the whole
  file. `lock --dry-run` relies on it: it warns and diffs against no baseline
  rather than rendering a hostile entry. Loosening that alphabet on the
  assumption that the printer neutralizes it would let a crafted lockfile print,
  say, a fake `Installed:` line.
- A dependency key a Galaxy server sends is held to the collection-name
  alphabet, not merely split at the dot, since the solver renders keys into its
  messages on an ordinary run.
- A manifest-declared identity in a signature attribution failure is truncated
  and rendered quoted, since an unquoted declared version could carry a line of
  its own.

A refused URL is printed through one display form, `helpers.URLForMessage`,
which cuts the query, the fragment and the userinfo and then truncates. The
cuts are string scans, never a `url.Parse` round trip, because the values that
most need one - a bad port, a malformed IP literal, a bad escape, a control
character - are the ones the parser refuses, and printing the raw string after
a failed parse would leak the password. The userinfo cut drops everything
through the last `@` of the authority, whose scan stops at the first `/`, `?`
or `#`, so the cuts compose in any order. One leak is known and accepted: a
userinfo containing `/` is printed whole, and one containing `?` or `#` is
printed truncated, since `https://h/x@y` is an ordinary URL with an `@` in its
path; `url.Parse` refuses all three spellings, so such a credential never
reaches a request, only a message. The fragment is cut because it never reaches
the wire or the filesystem (`file:///tmp/a#b.asc` opens `/tmp/a`). A URL
persisted or rendered rather than fetched loses its query and userinfo
(`helpers.WithoutCredentials`), while `helpers.WithoutQuery` has to stay a cut
of its own, since signature sources are persisted with only their query
removed.

The attacker-influenced strings operator messages render - a signature
origin or source, a `MANIFEST.json` or `galaxy.yml` identity, a refused
download or metadata URL, a role's name, version and meta values - are bounded
at 512 bytes by `helpers.TruncateForMessage`, which keeps the prefix and
appends `... (N bytes)`, so the truncation is visible. Without it such a field
is bounded only by the 16 MiB metadata cap or the 64 MiB manifest scan.

The origin a signature failure names for each blob has its userinfo and query
cut and is then truncated, in that order: truncating first can drop the `@` that
ends a long userinfo, after which the cut finds none and renders the
credential's prefix. A server-carried origin is bounded where it is produced,
because its href is chosen by the server or the snapshot and is rendered once
per failure for every blob of the collection, so an unbounded one multiplies
into hundreds of MiB of error text.

`tree` and `explain` print lockfile fields verbatim - name, version, source,
sha256, deps, and a role's repository, ref and commit - and a lockfile may be
hand-edited or come from anywhere, so each wraps its writer in
`safeout.NewWriter` before printing anything, and every helper it calls writes
through that writer. A new command or helper that prints lockfile fields must do
the same.

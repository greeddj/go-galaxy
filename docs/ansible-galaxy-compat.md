# Compatibility with ansible-galaxy

Drop-in means the same `requirements.yml`, the same `ansible.cfg` keys and the
same `ANSIBLE_*` environment variables, for the `install` subset - collections
and roles - the README's [Scope](../README.md#scope) names. It does not mean identical
behavior everywhere: each deliberate difference is called out below rather
than left to be discovered in CI.

Token handling is a second surface where that sentence splits, though less
sharply than signature verification does below, where the four `[galaxy]`
keys are refused outright: a `url` or a `validate_certs` is still read here
exactly as ansible reads it, and only its pairing with a token you supplied
is refused. What an *unauthenticated* run reads is drop-in: `[galaxy] server`
and every `[galaxy_server.<id>]` url come from a discovered file exactly as
ansible takes them. That is drop-in in what such a run reads, not in
everything it then does - the deliberate differences below (one server rather
than a union, a fail-closed 401/5xx) apply to an unauthenticated run too. An
*authenticated* run differs in one more way, deliberately and loudly:
go-galaxy refuses to pair a token with a server URL a file supplied -
unconditionally for `[galaxy] server`, and for a `[galaxy_server.<id>] url`
unless that same section also supplied the token - which ansible does not
check at all either way - see [Galaxy servers and
authentication](servers-and-auth.md#galaxy-servers-and-authentication) and its `--token`
section for the rule, its cost, and its remedies.

Signature verification is the one other surface where that "same `ansible.cfg` keys
and the same `ANSIBLE_*` environment variables" sentence splits in two:
`ansible.cfg`'s four `[galaxy]` signature keys are refused (see [Signature
verification](signatures.md#signature-verification) for why), while all four
`ANSIBLE_*` environment variables that configure the same settings are
honored. A reader who only takes away "`ansible.cfg` configures none of it"
would wrongly conclude the environment names are dead too - they are not.

## Deliberate differences

- **A collection comes from one server, not from a union.** ansible queries
  every configured server and merges the results; go-galaxy walks
  `server_list` in order and the first server that has the collection owns it
  for the whole run. Merging means the same `namespace.name@version` can
  arrive from two servers with different bytes, with an arbitrary tie-break
  deciding which one you install.
- **A failing server stops the run instead of being skipped.** Only a 404
  means "this server does not have it, try the next". A 401/403, a retryable
  5xx that survives the retry budget, another 5xx, or a transport failure all
  abort the run instead - the first two naming the server, the rest reporting
  the underlying failure. ansible swallows those and moves on, which turns a wrong token or a
  five-minute hub outage into an install from the public Galaxy - dependency
  confusion by accident.
- **A token is never handled on terms an ansible.cfg file chose - not the
  server URL it is sent to, and not whether that server's certificate is
  checked.** ansible has no such rule: whatever address `[galaxy] server` or
  a `[galaxy_server.<id>]` section names, and whatever that section sets
  `validate_certs` to, any token you have configured simply goes there over
  whatever connection results. go-galaxy refuses both pairings instead - a
  file-sourced URL, or a file-relaxed TLS policy, receiving an
  operator-sourced token - because the alternative is worse than a refused
  run: an attacker who can only commit a repository file, never touch your
  credential, still gets to choose where it is sent, or to strip the
  authentication of the connection it travels over. Silently dropping the
  token instead of refusing would not be safe either - an origin they chose
  that simply answers anonymously would then install its own, or the public
  Galaxy's, content in place of the private collections you meant to fetch,
  the identical dependency-confusion shape the previous bullet already
  refuses to risk. See [Galaxy servers and
  authentication](servers-and-auth.md#galaxy-servers-and-authentication) for the exact rule and
  its remedies.
- **`--timeout` is a no-progress budget, not a total-transfer cap.** It bounds
  the wait for response headers and the gap between two body reads, so a large
  download that keeps streaming is never cut off by it, however long it takes.
  ansible's own `--timeout` behaves the same way; what changed here is that
  go-galaxy used to apply it to the whole response as well. The gap it leaves -
  a server dribbling a few bytes into every idle window makes progress on every
  read and so never trips it - is closed by separate fixed ceilings on the whole
  acquisition, described under [install options](cli.md#install-options). A transfer
  that does stall reports `network read stalled` and exits `4`, or `5` when it
  fails one collection of an install; it is never reported as an interrupt.
- **Signature verification runs no external process.** This tool verifies
  OpenPGP detached signatures in pure Go and executes no `gpg` process at all,
  so a keyring has to be key material it can read directly - a GnuPG keybox
  (`.kbx`, the container a default GnuPG installation writes) is refused by
  name, naming the `gpg --export --armor` command that produces a keyring it
  can read instead. See [Signature verification](signatures.md#signature-verification)
  below.
- **`--disable-gpg-verify` is honored, and the divergence it creates is made
  loud rather than silent.** Switching verification off while a keyring is
  configured can print a warning from two places on the same run: once at
  startup, naming the configured keyring, and once more - only if some
  requirements root actually declares `signatures:` - naming the first such
  collection whose sources will not be checked.
- **A declared signature source is deduped and ordered per collection, never
  cached across the whole run.** Within one collection, its own declared
  sources are gathered in the order `requirements.yml` wrote them, then
  whatever the server offered alongside the artifact, with duplicate source
  URIs collapsed inside that one list. Nothing caches a fetched signature
  blob across collections: two roots naming the same source URI fetch it
  twice, and a collection recovered through the bounded evict-and-refetch
  path (see [Signature verification](signatures.md#signature-verification)) gathers every
  one of its sources again from scratch.
- **Once at least one signature verifies, a collection's archive is checked
  against its own `FILES.json` listing in both directions rather than merely
  extracted.** Every file, symlink and hardlink entry the archive carries must
  be listed, and every file `FILES.json` lists must hash to the digest it
  declares; see [Signature verification](signatures.md#signature-verification) for the two
  exemptions this check makes. That precondition is the whole of it, and it is
  the reason this bullet is not a claim about every run: a run with no keyring
  configured - the out-of-the-box state - or one under `--disable-gpg-verify`
  verifies nothing, and therefore checks no listing either.

- **A git source is built from a commit here, not cloned by a git binary.**
  ansible shells out to `git clone` and `git checkout` with whatever the
  runner's git configuration, credential helpers and `~/.ssh/config` supply,
  copies the checkout into the collections path, and records nothing about
  where it came from. go-galaxy speaks the git protocol itself over https or
  ssh, wants the commit the ref resolved to (never the name, so a branch
  moved between the advertisement and the fetch cannot substitute content),
  reads that commit's tree without a worktree or a checkout, builds the same
  `MANIFEST.json` and `FILES.json` an `ansible-galaxy collection build` would
  into an artifact, and from there on treats it exactly like a downloaded
  one: cached under a key that carries the commit, extracted by the same
  extractor, recorded with its commit in the `.info` directory's
  `go-galaxy.yml`, and pinned in the lockfile. The consequences, each deliberate:
  - The pin is the commit. `lock` records `type: git`, the repository URL,
    the ref as written, the commit and the subdir, and no `sha256` - the
    artifact is rebuilt deterministically from the commit, and the gzip
    bytes of a rebuild depend on the toolchain, so a digest over them would
    fail a frozen install for no reason an operator could act on. A lockfile
    with a git entry is written as `schema_version: 2`; one without stays
    at `1`, byte for byte.
  - `version:` on a git entry is a ref - a branch, a tag, a qualified
    `refs/heads/...` or `refs/tags/...`, or a full forty-digit commit - and
    defaults to `HEAD`. An abbreviated commit is refused (spell it in full,
    or a branch that happens to be hexadecimal as `refs/heads/<name>`), and
    a name that git itself would refuse to create is refused before any
    request. An unqualified name that is both a branch and a tag resolves to
    the branch, with a warning, as `git checkout` would.
  - The collection's version is its `galaxy.yml` `version:` and has to be an
    exact `MAJOR.MINOR.PATCH`; its namespace and name have to match the
    collection-name alphabet. ansible installs a missing or non-semver
    version as `*`. Here the run is refused naming the repository and the
    remedy, because the install path, the cache key and the lockfile all
    need an exact version.
  - `build_ignore` is honored with Python `fnmatch` semantics (so `*`
    matches `/`); `manifest:` directives are refused with a usage error
    naming `build_ignore` as the alternative. A directory carrying both a
    `galaxy.yml` and a `MANIFEST.json` is refused, where ansible silently
    prefers the `MANIFEST.json` for a git checkout. A `MANIFEST.json` alone
    is accepted and the tree is rebuilt, as ansible rebuilds it.
  - `dependencies:` keys must be `namespace.name` and resolve through the
    configured Galaxy servers (or another git source of the same run that
    provides that name). ansible incidentally accepts a git URL or a local
    path as a dependency key; here it is refused as an invalid dependency
    key.
  - A git collection is the single candidate for its `namespace.name`: a
    Galaxy dependency that constrains it is satisfied by the git version or
    fails the resolve with a proof. ansible lets whichever requirement
    resolvelib happens to see first win, silently.
  - One repository may hold several collections and is searched exactly as
    ansible searches it: a `galaxy.yml` in the target directory (the root,
    or the `#subdir`) is one collection, otherwise every immediate child
    directory with a `galaxy.yml` or a `MANIFEST.json` is one, and all of
    them install unless the entry names one (`name: namespace.name` beside
    a git `source:`). `#<subdir>` and `,<ref>` keep ansible's order: the
    comma is split first, then the fragment, so `<url>#sub,main` is ref
    `main` under `sub`, while `<url>,main#sub` asks for a ref named
    `main#sub` and fails at the remote, as it fails for ansible at
    checkout.
  - Credentials come from the environment, bound to a host (see [Git
    sources and authentication](servers-and-auth.md#git-sources-and-credentials)).
    No credential helper, `~/.netrc`, `~/.gitconfig` or `~/.ssh/config` is
    read; a credential in the repository URL is refused; the certificate of
    an https repository is always verified (`SSL_CERT_FILE`/`SSL_CERT_DIR`
    for a private CA, no `ignore_certs` equivalent); the host key of an ssh
    repository must be in known_hosts, with no trust-on-first-use; an ssh
    URL names its login explicitly (`ssh://git@host/...`) rather than
    defaulting to the local user; a redirect off the origin is refused.
  - The fetch is shallow (depth 1) when the remote advertises `shallow`, a
    direct fetch by hash when it advertises `allow-reachable-sha1-in-want`,
    and a full fetch of the ref, or of every tip, otherwise. ansible clones
    with `--depth=1` only for `HEAD` and in full for any named ref.
    Submodules are never fetched (ansible never initializes them either);
    their entries are skipped with a warning.
  - Symlinks are kept only when they point inside the collection; one that
    points outside is skipped with a warning, a dangling one fails the
    build, and a `galaxy.yml` or `MANIFEST.json` that is itself a symlink
    does not mark its directory as a collection (ansible's `isfile` follows
    the link). A tree entry named `..`, `.git`, or anything that is not a safe
    path element, and two entries whose names differ only in case, are
    refused. `galaxy.yml` itself is not installed, as with ansible.
  - A run resolves a branch or tag once and replays the commit it found
    until `--refresh` asks again (editing the git line itself - its URL, ref
    or subdir - is a new question; editing other entries is not);
    `--offline` replays a recorded pin or fails. A dry run still fetches the
    repository - the collections it holds cannot be known otherwise - and
    writes nothing to the cache.
  - `file` and `dir` sources remain refused at load; a `url` source is
    supported, with its own divergences (see the url bullet under Notes).

Resolution itself is stricter than ansible's: a constraint set with no solution
is a failure with a proof, not a lenient pick. See [Exit codes](exit-codes.md#exit-codes) for
what each failure class exits with.

## Differences a migration runs into

Everything below is deliberate too, and accepted for this tool's CI scope;
what separates it from the list above is subject matter rather than posture.
Those differences are about servers, credentials and artifact trust, while
these are what a pipeline moving off `ansible-galaxy` actually meets: what a
resolve picks, which defaults differ, which features are absent, what an exit
code means, and what an installed tree looks like. Every "ansible does X"
claim in this subsection is read against ansible-core 2.21.2. One of them is
a property rather than an inventory and is stated once here: a flag this tool
does not define is a usage error naming the flag and exits `2`, which is how
`-U`, `--force`, `--force-with-deps` and `--pre` surface. A positional argument
surfaces the same way: ansible takes collection and role names, paths and URLs
on the command line, while here they come only from the requirements file, so
`ansible-galaxy collection install ns.name` rewritten as `go-galaxy install
ns.name` - or left as `go-galaxy collection install ns.name`, whose first word
names no command and so reaches `install` as an argument - exits `2` naming
the arguments it refused, rather than installing the requirements file
without them.

- **The resolve decides the version, not what is already installed.** ansible
  prefers a collection that is already present and does not query the Galaxy
  API for it, upgrading only under `-U` and reinstalling only under
  `--force`/`--force-with-deps`. Here resolution never consults the installed
  tree at all: nothing on disk can influence which version is chosen. The
  installed tree is read only afterwards, to decide whether the resolved
  version still has to be installed - the record is looked up under the exact
  resolved `<namespace>.<name>@<version>`, and a record that exists must
  still name the same install path, name the same server the collection now
  resolves from, and agree with any lockfile pin, with the extract marker and
  the `GALAXY.yml` sidecar - both in the version's own
  `<namespace>.<name>-<version>.info` directory - checked on top. A record that satisfies all of that skips the install
  rather than downloading anything again. The sidecar is the condition that
  surprises, because it is version-scoped and is not part of the collection's
  own content: deleting or renaming that `GALAXY.yml` forces a full
  re-download and re-extract even though the record, the install path, the
  server, the pin and the extract marker all still agree. It is read rather
  than merely found, and has to name this collection at this version, so a
  truncated write and a document left by another version cost the same
  re-download - existence alone was never evidence that the tree and the
  provenance record beside it belong together. Two disagreements do not: a
  sidecar whose `server` fell behind the record, and a sidecar outside
  ansible's schema (next bullet). Either is
  rewritten in place, since the record has already been shown to name this
  collection and this source and a re-download would change no installed
  byte. That repair is the only write a skipped install makes, and it
  happens only when the file on disk is not already what this tool writes. The consequence runs both ways: the
  install path carries the namespace and the name but not the version, so a
  resolve that lands lower installs the lower version over a newer tree, and
  there is no "prefer what is there". A rerun is not by itself a new resolve,
  though - with `requirements.yml`, the effective server list and `--no-deps`
  all unchanged, the recorded resolution is replayed and the same versions
  come back; `--refresh` (see [install options](cli.md#install-options)), or a
  change to any of those three inputs, makes it resolve again, and only then
  can an open constraint land somewhere new. `--no-deps` counts as an input
  because it is folded into the same signature the other two are: toggling it
  between two otherwise identical runs re-resolves. To hold versions still across runs, lock them - see
  [lock](cli.md#lock).
- **`GALAXY.yml` is written for every install, and held to ansible's schema.**
  ansible-core writes that sidecar only for a collection a Galaxy server
  served; go-galaxy writes it for git and url installs too, because its
  install-skip check and `outdated` read it. ansible validates the file
  against a closed schema - `download_url`, `format_version`, `name`,
  `namespace`, `server`, `signatures`, `version`, `version_url` and nothing
  else - on every command that reads the installed tree, so the document
  carries exactly those keys, and `signatures` is always a list: a null
  there passes ansible's validation and then makes `ansible-galaxy
  collection verify --offline` fail with a Python `TypeError`. For a git
  install `server` names the repository, and for a url install `server` and
  `download_url` both name the tarball. What such an install records beyond
  the schema - the commit, or the sha256 of the fetched bytes - goes into a
  file of its own in the same directory, `go-galaxy.yml`, which ansible does
  not read and removes along with the directory when it reinstalls the
  collection. Earlier releases wrote those keys into `GALAXY.yml` itself, and
  ansible answered each command with `[WARNING]: url_sha256. Supported
  parameters include: ...` (or `git_commit`) and ignored the file. The first
  `install` after upgrading extracts every collection an earlier release
  installed once more (see the extract marker below), which writes the
  sidecar anew; until then `outdated` still reads the key where an earlier
  release left it.
- **Prereleases are excluded and admitted on different rules than ansible's.**
  Stricter in one direction: a collection publishing only prerelease versions
  satisfies no plain constraint here, so the resolve fails with its proof plus
  a hint naming that collection and pointing at an exact pin or a `>=X.Y.Z-0`
  floor, where ansible-core 2.21.2 installs it, since every published version
  stays a candidate there. Looser in the other: a constraint carrying a
  prerelease operand admits prerelease versions for that whole constraint, so
  a `>=1.0.0-0` floor also matches a later prerelease such as `2.0.0-rc1`,
  which ansible filters out. Take away both halves rather than either one -
  prereleases are not unreachable here, since an exact pin on one installs it
  and a `-0` floor admits them; they are reached by naming them in the
  constraint, and `--pre` is not a flag this tool defines.
- **`requires_ansible` is not checked.** ansible reads a collection's
  `meta/runtime.yml` and refuses to install one whose `requires_ansible`
  excludes the running core version. go-galaxy never reads that file: it
  installs no ansible-core and so has no version to check a requirement
  against, and a collection ansible would refuse installs cleanly here. The
  check does not disappear, it moves - from install time to play time,
  surfacing when the playbook runs rather than when the collection lands.
- **`collections_path` is one path, not a search list.** The value is split on
  `:` and only the first entry is used, whichever route it arrived by:
  `[defaults] collections_path`, `ANSIBLE_COLLECTIONS_PATH`, or
  `-p`/`--download-path`. The ignored entries are named in one stderr warning
  and nothing else ever looks at them - `-p "a:b:c"` warns `collections_path
  lists multiple paths; using "a" and ignoring the rest: [b c]` and installs
  into `a`. ansible searches every entry when deciding what is already
  installed, so a collection installed under a later entry is invisible here
  and is installed into the first one. The default differs as well:
  `.collections`, project-local, against ansible's `~/.ansible/collections`.
- **No token file is read.** ansible falls back to a token file when no server
  section supplies one - `~/.ansible/galaxy_token`, relocatable through
  `[galaxy] token_path`. go-galaxy reads no token file at all: `token_path` is
  one of the `[galaxy]` keys it drops without a word - unlike the four
  signature keys, which are named in a warning - and no default path is
  consulted. The routes that do supply a credential are
  `--token`/`GO_GALAXY_TOKEN`, a `[galaxy_server.<id>] token`, and
  `ANSIBLE_GALAXY_SERVER_<ID>_TOKEN` - see [Galaxy servers and
  authentication](servers-and-auth.md#galaxy-servers-and-authentication). Both directions are
  worth planning for: against a hub that requires a credential the run fails
  loudly and names the server (exit `4`, the fail-closed 401/403 behavior
  [Deliberate differences](#deliberate-differences) already describes), while
  against a server that serves anonymously, as the public Galaxy does, the run
  simply proceeds unauthenticated with nothing said.
- **The request-timeout default is tighter.** `--timeout` defaults to `30s`
  here, against ansible-core 2.21.2's 60s. Both tools read the same
  `ANSIBLE_GALAXY_SERVER_TIMEOUT` and the same `[galaxy] server_timeout` in
  `ansible.cfg`, in the same order - the flag, then the variable, then the
  file - so a pipeline that sets either gets the same number in both, and
  only a pipeline relying on the default gets a tighter budget here. A hub
  slow to answer headers can therefore sit inside ansible's default and
  outside this one, with any of those three as the remedy. Two narrower
  differences remain. The file's value is read by the grammar `--timeout`
  uses, so `90s` works here where ansible refuses anything but whole seconds,
  and a file both tools read should keep to those. And a per-server
  `timeout` in a `[galaxy_server.<id>]` section is not read: this tool has
  one budget for the whole run, so that key is named in the warning about
  unsupported keys and otherwise ignored. What the budget bounds is unchanged
  - see the `--timeout` bullet above and [install options](cli.md#install-options).
- **The exit codes are not ansible's, and the same numbers mean different
  things.** ansible-core 2.21.2 uses a small flat set: `1` generic, `4` parser
  error, `5` options error, `99` interrupt, `250` unexpected. go-galaxy's
  codes are class-specific and do not line up with those - `4` and `5` exist
  in both with unrelated meanings, and an interrupt here is `130`/`143`/`129`
  rather than `99`. A script branching on ansible's numbers therefore misreads
  a go-galaxy run silently. See [Exit codes](exit-codes.md#exit-codes) for what each code
  means here.
- **An installed tree looks different on disk.** An installed regular file
  carries the mode it was unpacked with, and that mode has no write bit for
  anyone, because the write bits are stripped once - when an artifact is
  unpacked - so nothing downstream hands out a writable copy.
  `ansible-galaxy`'s own installed files are writable by their owner, so an
  in-place edit that worked after an ansible install fails here with a
  permission error. The reason is that an install can share the unpacked bytes
  with every other install that references them, so a writable installed file
  would alias a write into all of them. Directories are not read-only, so this
  is about editing an installed file, not about a frozen tree; edit a copy
  outside the collections tree instead. Each installed collection also has a
  `.extract-done.<sha256>` marker this tool writes: it records a count of
  entries and directories plus those entries' total size, not a hash. An edit
  that changes any of those makes the next install re-extract the collection
  over it; an edit that preserves the edited file's exact byte length does
  not, and survives. The marker sits in the version's `.info` directory beside
  `GALAXY.yml`, not in the collection directory, because `ansible-galaxy
  collection verify` reports every file there that the collection's
  `FILES.json` does not list and exits `1` over it. Earlier releases put it in
  the collection directory, so verify failed on every collection they
  installed, and the first `install` after upgrading extracts each of those
  once more - from the cache, with no download while the artifact is still
  there. An extraction also removes every other version's `.info` directory of
  the same collection, as `ansible-galaxy` does whenever it installs one from
  an artifact: that directory is scoped to a version and the collection
  directory is not, so a marker left beside an earlier version would outlive
  the tree it counted, and installing that version again would take it for
  proof of an install whenever the tree now in place happened to share its
  tally.

## Roles

Roles are installed from the same `requirements.yml` by the same `install`,
as `ansible-galaxy install -r` does, and land under `roles_path` one
directory per role with `meta/.galaxy_install_info` written beside the
role's meta, so `ansible-galaxy role list` reads the result. The mechanism
differs from ansible's at the root - ansible downloads a Galaxy role as a
GitHub tarball of the tag and clones a git role with the `git` binary, while
this tool fetches both through its own git client and builds a deterministic
artifact it caches, pins and extracts like a collection's - and every
consequence of that, and every other deliberate difference, is listed here.
"ansible does X" is read against ansible-core 2.21.2.

- **The `roles:` grammar is ansible's, judged stricter at load.** The string
  form `src[,version[,name]]`, the mapping form with `src:`, `scm:`,
  `version:` and `name:`, the old-style `role:` key, the `scm+url` prefix
  and `*` for "no version" are all read as `role_yaml_parse` reads them.
  What ansible would accept and this tool refuses at load, with the usage
  code (`2`) and before any request: `include:` of a second file, an `scm`
  other than `git` (ansible shells out to `hg` for the other value; this tool
  executes no process), a non-http or non-`.tar.gz` URL or a local path as
  `src:` (an http(s) `.tar.gz` URL is a url role - see the url bullet under
  Notes), a `#subdir` fragment on a git role, a `source:`, `signatures:`
  or `type:` key on a role entry (each would silently change what the entry
  means), two entries installing into one directory (ansible installs the
  second over the first), and a credential in a repository URL. A key ansible
  drops without a word is dropped here with a warning.
- **A Galaxy role name has exactly one dot.** ansible splits `owner.role` at
  the *last* dot and lets the owner carry dots of its own; no GitHub login
  can, so this tool asks for the one unambiguous spelling, each half matching
  `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`, and refuses the rest rather than
  guessing which dot was meant. A three-part `namespace.collection.role`
  named as a dependency is a collection's role and is skipped with a warning,
  as ansible skips it; a dependency with no dot and no scm is a local role
  ansible never looks up either, and is skipped the same way.
- **The github.com special case matches the host, not the string.** ansible
  promotes an http(s) `src:` to a git repository when the text contains
  `github.com` anywhere and does not end in `.tar.gz`; this tool requires the
  parsed host to be exactly `github.com`, so a tarball host that merely
  mentions it is not promoted. A github.com URL that does end in `.tar.gz` -
  a release asset, an archive link - is a url role here, downloaded directly
  and pinned by its sha256.
- **A Galaxy role is fetched by git at the tag, not as the GitHub tarball.**
  The v1 API is asked the same two questions ansible asks - the role record,
  then its version list - on the configured server list in order (a server
  without v1, such as an Automation Hub, is skipped; no v1 anywhere exits
  `2` naming the servers; a role no v1 server knows exits `3`). The record's
  `github_user` and `github_repo` are validated and composed into
  `https://github.com/<user>/<repo>`; its `download_url` is never read.
  The repository is then fetched at the chosen tag through the git client,
  as `refs/tags/<tag>` spelled in full - so a branch that happens to share a
  tag's name can never be fetched in its place, where the tarball URL ansible
  builds from the bare name leaves that choice to GitHub - and from there the
  role is a git role: built into an artifact, cached under its commit, pinned
  in the lockfile by commit. An all-digit tag (a date) is a tag here as it
  is for ansible; only the unqualified spelling in a `roles:` entry reads
  seven to thirty-nine hex digits as an abbreviated commit. Two consequences are
  visible. The Galaxy token configured for the server never reaches
  github.com (ansible sends none there either, but reaches it through the
  tarball URL the server supplied); a `GO_GALAXY_GIT_*` binding for
  github.com applies when one is configured. And files a repository marks
  `export-ignore` in `.gitattributes` **are** installed here, where ansible's
  tarball - produced by GitHub's `git archive` - honors the attribute, because
  nothing here reads attributes.
- **Version selection is ansible's, with one leniency and one tightening.**
  No version asked for: the highest tag under distutils' `LooseVersion`
  order, where ansible's tie between equal tags falls to server order and
  this tool's to the lexicographically greater name; two tags `LooseVersion`
  cannot compare (a number against a word) fail with ansible's own remedy,
  "specify an explicit version" (exit `3`). No tags: the record's
  `github_branch`, else `master`. A version asked for must be a listed tag,
  or the default branch itself - ansible accepts only the literal `master`
  unlisted, this tool accepts the branch the record names as well as
  `master`; and when the server lists no tags at all, any version asked for
  is passed to the repository as a ref. A version that is not listed exits
  `3` naming what was listed. A `commit_sha` the v1 API recorded for the tag
  that disagrees with what the repository advertises is a warning, and the
  repository wins; ansible never compares the two.
- **The repository root is the role, and only the root.** `meta/main.yml`,
  else `meta/main.yaml`, each a regular file, marks it; a tree carrying both
  is refused rather than read through the first, `meta/main.json` is not
  read, and a nested `meta/main.yml` is never searched for - ansible's
  shortest-parent scan exists for archives built by hand. A repository with
  no meta at its root fails the run as not a role (exit `2`). A
  `meta/.galaxy_install_info` a repository committed by accident is left out
  of the artifact: the install writes its own after materialization, where
  ansible extracts the committed one and then overwrites it.
- **Dependencies are walked breadth-first, first wins, and the loser is named.**
  `dependencies:` in `meta/main.yml` plus `meta/requirements.yml` are
  installed transitively in every shape ansible reads them, and the first
  request for an install name wins, as in ansible; a later request for the
  same name with a different source or version is ignored *with a warning*
  where ansible is silent. Which request is first is decided by the file's
  and the meta's declaration order, never by which fetch finished. `--no-deps`
  stops the walk. One run may discover at most 1000 roles.
- **An existing role directory is converged, not skipped.** ansible skips an
  already-installed role at a different version with a warning unless
  `--force`; this tool replaces a directory it installed itself (its extract
  marker) when the source changed, replaces one `ansible-galaxy` installed
  (`meta/.galaxy_install_info` without the marker) with a warning, and
  refuses one that carries neither - "appears to already exist", ansible's
  own refusal - with the install-failure code (`5`), because replacing it
  could destroy a role somebody wrote by hand.
- **The installed tree looks like ansible's, with this tool's marker and
  modes.** Entries sit at the top level of the role directory with no
  `MANIFEST.json` or `FILES.json` (a role has neither), `.galaxy_install_info`
  carries `install_date` in ansible's `%c` format (UTC wall clock) and
  `version` as the tag, branch or commit asked for (`HEAD` resolves to the
  branch name), and the directory also carries the `.extract-done.<sha256>`
  marker and the read-only file modes described under [Differences a
  migration runs into](#differences-a-migration-runs-into), which is what
  marks it as this tool's for `cleanup`.
- **The pin is a commit, and the lockfile is schema 3.** `lock` records each
  role as `type: galaxy` or `type: git` with the version as asked (a tag, a
  branch or a commit - not a semver), the ref, the commit, the repository,
  the Galaxy name and server for a Galaxy role, and the install names of its
  dependencies, and no `sha256`, for the reason a git collection has none. A
  url role is the exception: its entry is `type: url` with the tarball URL
  as its source and the origin bytes' `sha256` as its pin, and a file
  holding one is `schema_version: 4`. A file holding a role is written as
  `schema_version: 3`; one without roles is byte-identical to what it was
  before roles existed. ansible has no lockfile for roles at all.
- **`roles_path` is one path, not a search list, and its default is
  project-local.** As with `collections_path`: the first entry of
  `[defaults] roles_path`, `ANSIBLE_ROLES_PATH` or `--roles-path` is used,
  the rest are named in a warning, and the default is `.roles` rather than
  ansible's `~/.ansible/roles`. That warning, and the one about a roles path
  equal to the collections path, are held back unless the requirements file
  carries a non-empty `roles:` block - the condition under which the setting
  is read at all. ansible has no equivalent, since it has no single file
  declaring both kinds of requirement to a single command.
- **Roles have no signatures and no `sha256`.** The v1 API offers none and
  ansible verifies none; here the commit check at every rebuild - a
  repository that serves a different commit for a pinned role fails with the
  integrity code (`7`) - stands in for attribution.
- **`ansible-galaxy role` subcommands have no counterpart.** There is no
  `role init`, `role remove`, `role search`, `role import` or `role list`;
  `install` installs, `cleanup` removes what no project reaches, and
  `tree`/`explain`/`outdated` read the lockfile. `--force` is not a flag this
  tool defines, and `-p` means something else here: it is `--download-path`,
  the collections path, where `ansible-galaxy role install -p` names the
  roles path - that is `--roles-path` here.

## Notes

Two shapes ansible accepts are handled here in opposite ways, which is worth
knowing before a `requirements.yml` written for ansible is pointed at this tool.

- **A `url` collection source installs, with divergences each in the
  stricter direction.** An http(s) URL in the name position (or an explicit
  `type: url`) is downloaded directly, its identity and dependencies read
  from the artifact's own MANIFEST.json, and the collection resolved as the
  single candidate for its `namespace.name`, exactly as a git collection
  is. The lockfile pins it by the origin bytes' sha256 (schema 4), a pin a
  git entry cannot carry, and the pin is enforced on every install, not
  only under `--frozen`. Where ansible ignores a `version:` beside a url
  source, here it is an assertion: the value must be exact and must equal
  the version the MANIFEST declares, or the run fails with the resolution
  code (`3`). A `signatures:` key on a url entry is refused (ansible does
  not apply user signatures to url sources either; here the dead key is
  named rather than dropped), and so are a `source:` or `namespace:` key
  beside a url name, a credential in the URL, and a `#fragment`. An
  optional Bearer token binds per origin and path prefix through
  `GO_GALAXY_URL_*` (see [Servers and auth](servers-and-auth.md#url-sources-and-credentials)),
  a capability ansible has no counterpart for; a redirect that leaves https
  for plaintext http is refused, where ansible follows it. A `--refresh`
  re-downloads the URL - there is no cheaper probe - and unchanged bytes
  keep the pin; `outdated` reports a url entry as current by construction,
  since a content-addressed pin has no version feed to compare against.
  A caching-proxy URL - `http://front/<upstream-url>`, where a front host
  reads the rest of the request path as the URL it fetches and caches - is
  accepted, with one rule ansible does not impose: the embedded upstream URL
  must itself be a valid url source spelled in its canonical form (lower-case
  scheme and host, no default port), so one upstream artifact keeps one
  spelling, one pin and one credential-match reading.
- **A `file` or `dir` collection source fails the whole file, rather than
  being skipped.** A `type:` other than `galaxy`, `git` or `url`, or -
  where no `type:` is given - a name that reads as a source rather than as
  `namespace.name`, a git pointer or an http(s) URL, meaning anything
  containing `://` outside http(s) or starting with `/`, `./`, `../` or `~`
  (a `git+` or `git@` prefix is a git source), is refused at load with the
  usage code (`2`), naming the offending value, before any request is made.
  The messages are `unsupported collection type "file" (only galaxy, git
  and url are supported)` and `unsupported collection source "..." (only
  Galaxy API, git and url sources are supported)`. ansible installs these;
  here the run does not start.
- **A bare top-level list is a list of collections, not ansible's legacy
  roles file.** `ansible-galaxy install -r` reads a file that is a bare list
  as roles; here it is read as collections, and a `src:` or `scm:` key inside
  a collection entry is refused naming the `roles:` key, so a legacy roles
  file pointed at this tool fails at load rather than installing the wrong
  kind of thing. Roles always go under `roles:`. Earlier versions of this
  tool ignored that key with a warning and exited `0`; a `roles:` list is
  now decoded, installed, and a malformed entry in it fails the file.

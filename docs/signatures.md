# Signature verification

`install` and `warm` can verify a collection's detached OpenPGP signatures
against a keyring before it is extracted. `lock` and `outdated` never verify
anything - neither of them registers the flags below at all, since neither
one writes anything into the collections tree for a signature to guard. It
is off by default and free when off: with no `--keyring` configured,
nothing is read, no HTTP client for signature sources is built, and nothing
is gathered.

## Turning it on

| Flag                                          | Environment                                | Ansible environment                             |
|:----------------------------------------------|:-------------------------------------------|:------------------------------------------------|
| `--keyring`                                   | `GO_GALAXY_KEYRING`                        | `ANSIBLE_GALAXY_GPG_KEYRING`                    |
| `--required-valid-signature-count`            | `GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT` | `ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT` |
| `--ignore-signature-status-code` (repeatable) | `GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE`   | `ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES`  |
| `--disable-gpg-verify`                        | `GO_GALAXY_DISABLE_GPG_VERIFY`             | `ANSIBLE_GALAXY_DISABLE_GPG_VERIFY`             |

`--keyring` names an OpenPGP keyring: armored or binary key material this
tool reads directly, never a GnuPG keybox (`.kbx`, the container a default
GnuPG installation writes) - a keybox is refused by name, with the
`gpg --export --armor` command that produces a keyring this tool can read
included in the refusal. This tool executes no `gpg` process; it verifies
signatures in pure Go.

**`ansible.cfg`'s four `[galaxy]` signature keys (`gpg_keyring`,
`required_valid_signature_count`, `ignore_signature_status_codes`,
`disable_gpg_verify`) are refused, but their `ANSIBLE_*` environment
variables listed above are honored.** A discovered `ansible.cfg` naming any
of the four is never read for its value: this program cannot establish
whether that file was authored by the operator or by the repository under
test, and a setting that can relax a verification check must not come from a
file whose author it cannot establish. `install`/`warm` warn about it once
per run, naming the file and the key names it carried - never their values,
since none were read; `lock`, `outdated`, `cleanup`, `hash`, `tree` and
`explain` never emit this warning, since none of them verify anything it
could apply to.

The refusal covers every `ansible.cfg`, however it was found, because no
test of who wrote the file holds up. A repository can ship `./ansible.cfg`; a
workflow's `ANSIBLE_CONFIG` usually names a repository-relative path, so the
operator picks the variable while the checkout picks the content; a
repository that also ships the workflow picks both; and on a self-hosted
runner one job can leave `~/.ansible.cfg` behind for every later job. Keying
on the discovery slot misses the `ANSIBLE_CONFIG` case, keying on the path
lying under the working directory misses a symlinked target and a run whose
working directory is not the checkout, and keying on file ownership misses
the runner case, since the earlier job wrote the file as the same user. An
environment variable is set by whoever configured the run rather than by the
checkout, so the flags and their variables stay the only source of a setting
that can relax verification.

An explicitly supplied but empty `--keyring` or
`--required-valid-signature-count` - the flag or any of its variables set to
the empty string - is refused (exit `2`) rather than read as unset. That is
what a CI job produces when it interpolates a secret that is withheld, as on
a pull request from a fork: an empty keyring would silently verify nothing,
and an empty count would silently replace a stricter configured value such
as `+all` with the default `1`. The other two settings accept an empty
value, since each empty meaning already keeps verification on: an empty
disable switch is false and an empty ignore list tolerates nothing.

`ANSIBLE_GALAXY_DISABLE_GPG_VERIFY` takes ansible's boolean spellings -
`true`/`false`, `yes`/`no`, `on`/`off` and `1`/`0`, case-insensitive and with
surrounding whitespace ignored - and is read only when neither the flag nor
`GO_GALAXY_DISABLE_GPG_VERIFY` set the switch. Empty reads as absent; any
other value is refused (exit `2`) rather than guessed. It is read apart from
the flag because the flag's own variable takes Go's boolean spellings, which
refuse `yes` and `off` by aborting the command: an environment already
exporting `no` for ansible would otherwise fail every `install` and `warm`.

## Keyring and signature file formats

The keyring's format is judged from its bytes, never its file name. A GnuPG
keybox is recognized by its `KBXf` magic at byte offset 8. A file carrying a
`-----BEGIN PGP PUBLIC KEY BLOCK-----` (or `PRIVATE KEY BLOCK`) header
anywhere, behind a leading comment included, is read as armor, and anything
else as binary packets. Every armor block contributes its keys, so
`cat teamA.asc teamB.asc > keyring.asc` trusts both teams; each export has to
end with a newline (gpg's does), and an armor opening line that does not
start a line of its own - two exports glued together - is refused with that
remedy. The load also refuses a file over 64 MiB rather than truncating it,
which would silently drop keys; an armor block that is not key material,
named by its type rather than skipped; any secret key material, naming
`gpg --export --armor` as the fix; and a file that parses to no keys at all.

What may sit in the file is decided by packet tag, never by whether the
parser could read the packet. A keyring may hold only what a transferable
public key is made of - public keys and subkeys, user IDs and user
attributes, and signatures - plus GnuPG's ring-trust packet, so a raw
`pubring.gpg` loads, and RFC 9580 padding. A secret key or subkey packet
refuses the file even where the parser would have skipped it, and so does a
marker packet. The file may hold at most 4096 packets across all of its
armor blocks, each block also counting as one before it is decoded: a
minimal three-packet key exported on its own costs four, so 1024 such
exports concatenated load and 1025 do not. That is sized for a collection
publisher's keyring, not a distribution's, and a file past it is refused
whole. The framing check leaves a v3 signature alone, so a keyring carrying
the certifications PGP 2.x and GnuPG 1.x made on long-lived keys is not
refused for them.

A detached signature may hold signature packets and nothing else, at most 64
of them (one per signer), so a public key export offered as a signature is
refused. That limit counts packets inside one signature; the cap of 64
described below counts signatures gathered for one collection, and the two
only happen to agree.

## Required count and the vacuous pass

`--required-valid-signature-count` (default `"1"`) accepts four spellings: a
bare non-negative count (`0`, `1`, `2`, ...), the literal `all`, or either
prefixed with `+`. A bare count or bare `all` is satisfied **vacuously** by an
empty gather - a collection offering no signatures at all still passes -
because neither spelling asks for a floor an empty set can fail. Only the `+`
marker closes that: `+N` additionally requires at least one signature to
have verified, and `+all` requires the same on top of every checked signature
verifying. An operator who needs a signature actually required writes `+1`
or higher. The vacuous pass is ansible-galaxy's own verdict, kept on
purpose; go-galaxy departs from it only by warning (below).

The whole value has to match, the way ansible's own pattern requires:
nothing is trimmed and nothing is case-folded, so a count with a space
around it, `+ 1`, `++1`, `1all` and `ALL` are all refused, and only ASCII
digits make a count. Accepting a spelling ansible-galaxy refuses would let a
configuration work here and then fail under ansible-galaxy.

What the number counts is **distinct signing keys**, not signature files: two
signatures made by the same key count once, so `2` asks for two independent
signers and the same signature supplied twice can never stand in for a second
one. A counted spelling, bare or `+`, also stops the walk at its Nth distinct
signer, so signatures past that point are never checked and cannot fail the
run. `all` is the spelling with no such cutoff - it is the one that checks
every gathered signature, and the only one under which a signature that
fails with a status this run does not tolerate fails the run. The `+` marker
adds its one clause and nothing more: under a counted spelling, bare or `+`,
a failure beside enough verified signers does not fail the run (`+1` with
two failures and one success passes, as in ansible). Making it fatal there
would pass or fail a run depending on where the bad signature sits in the
list, since the walk stops at its Nth signer.

Two spellings are traps rather than choices. `+0` can never pass, under any
outcome: it is refused the moment nothing verifies (the strict clause) and
refused the moment something does (the equality clause), since there is no
strict form of "require zero" signatures. And `-1` - a spelling that means
"require every signature" in some ansible documentation - is refused by
name, naming `all` as the spelling to write instead; ansible's own grammar
accepts no negative number, so `-1` was never a working spelling to begin
with.

A run with verification on that gathers nothing to check for a collection -
because none was declared and the server offered none - still passes, under
any non-strict spelling including the default, but it is not silent about
it: it prints one warning naming the collection and the exact strict
spelling (`+1`, `+2`, ...) that would make that same pass fatal. A
collection whose every gathered signature failed with a status this run
tolerates draws the identical warning, but only where it actually passes:
under `all` or a bare `0`, never under the default or any stricter bare
count, where a tolerated failure still leaves the required count unmet and
the run fails instead. A cold API cache under `--offline` produces a
related but distinctly worded warning: when this run could not even learn
whether the collection carries signatures at all (no cached version
metadata, or the server failed to answer), the message says so explicitly
rather than reading like "this collection carries no signatures" - a run
that never learned whether a collection is signed has not learned that it
is unsigned either.

## Ignored status codes

`--ignore-signature-status-code` (repeatable) names a gpg status code this
run tolerates as a non-fatal signature failure - `BADSIG`, `NO_PUBKEY`, and
so on. The vocabulary is closed and three-way: eight codes are ones a
verdict from this tool can actually carry (`BADSIG`, `ERRSIG`, `NO_PUBKEY`,
`EXPKEYSIG`, `REVKEYSIG`, `EXPSIG`, `NODATA`, `BADARMOR`); two more
(`KEYEXPIRED`, `KEYREVOKED`) are gpg's other name for two of those eight and
are accepted as synonyms, so configuring either spelling covers both; and six
more (`MISSING_PASSPHRASE`, `BAD_PASSPHRASE`, `NO_SECKEY`, `UNEXPECTED`,
`ERROR`, `FAILURE`) are accepted and inert, since this tool holds no secret
key material and runs no `gpg` process to report on itself. A value outside
all sixteen is refused, naming the accepted list. What the one-line
announcement a verifying run prints at startup does **not** name is which
codes are ignored - only the keyring path and the required count - and the
six inert codes are accepted with no warning of their own, since tolerating
a failure that cannot occur changes nothing either way.

Each configured code is matched with surrounding whitespace trimmed and case
ignored, so the comma-separated
`ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES="BADSIG, NO_PUBKEY"` works as
written.

Because the ignore list keys on status, which status a failure gets is part
of the contract, and no failure is steered onto a status configured for
something else. A blank blob, or a well-formed armor envelope holding
nothing, is `NODATA`, where the OpenPGP library would report the empty
packet stream as an unknown issuer, an ignorable-looking `NO_PUBKEY`.
`BADARMOR` is only armor the decoder itself could not read. This tool's own
refusals - malformed packet framing, a packet that is not a signature, an
oversized armor header, an armored block of another type - are `ERRSIG`, and
so is any error it does not recognize: an unanticipated failure makes a
signature fail, never verify, and an entry written to tolerate `BADARMOR`
never stretches to cover these.

## `signatures:` in requirements.yml

A collection entry may declare `signatures:` as a single source string or a
list of them:

```yaml
collections:
  - name: community.general
    version: "11.1.0"
    signatures:
      - https://galaxy.ansible.com/community/general/signatures/foo.asc
      - file:///etc/pki/collections/community-general.asc
```

Each source has to be one of: an absolute `http` or `https` URL, or a
`file://` URL naming an absolute local path with an empty authority or
`localhost` only (`file:///etc/...` or `file://localhost/etc/...`). Every
source is validated where the file is read, before any request is made, as a
usage error naming the file rather than a per-collection worker failure: a
value that names nothing fetchable (no scheme, an unsupported scheme, an
opaque value, an `http`/`https` URL naming no host, or a `file` URL naming
another host or a relative path), one embedding a credential in its userinfo
(`https://user:pass@host/sig.asc`), a `signatures:` value that is neither a
string nor a list of strings, or more than 64 sources declared for one
collection, each fail the load. A source's own query string is still sent on
the request - it may be a presigned capability the source needs to be
fetchable at all - but it is cut from every message naming the source and
never persisted: it is cut from every source before the resolved
requirements spec reaches the store, and cut again whenever the snapshot is
saved, so an entry an older binary persisted uncut, which an
`install --frozen` run carries forward without rebuilding it, loses its
query on the next save. A run with nothing to save leaves such an entry as
it was.

A source is fetched over a client of its own, built with no server
configuration: it attaches no Galaxy token and relaxes no certificate check
for any origin, even one a configured server shares. A `signatures:` value is
written by whoever can commit to the repository, not by the operator, so a
client that attached credentials by origin would hand a hostile requirements
file a token-bearing request to a path of its choosing, or unverified TLS on
an origin the operator relaxed for their own server. This matches ansible,
which sends no credential and forces certificate validation when it fetches a
signature. Redirects are followed, with the `Referer` header dropped on
every hop, so a presigned query does not reach the redirect target.

Declaring `signatures:` with no keyring configured is a hard error (exit
`2`) naming the first such collection - the verdict earned by a requirements
file that asks for verification with nothing configured to verify against.
`--disable-gpg-verify` changes that: verification explicitly switched off is
not treated as a missing keyring, so the run proceeds, but it says so loudly
in up to two places on the same run - once when a keyring is configured and
the switch disables it anyway, and once more (only if some requirements root
actually declares `signatures:`) naming the first collection whose declared
sources will not be checked.

## What a verifying run does differently

- **The pin, then the signature, both before extraction.** For a
  lockfile-pinned collection, its recorded sha256 is checked first, then its
  signatures - only once both hold does anything get written into the
  collections tree, so a collection this run cannot attribute is never
  extracted where a playbook would find it.
- **A verifying run cannot take the metadata-free cache-hit fast path.**
  Normally, an already-cached artifact with no metadata pushed by the
  resolver installs with no per-collection metadata request at all. A
  server's own signatures ride on that same version-metadata document, so a
  verifying run gives up that shortcut: a collection signed only by its
  server would otherwise pass vacuously on a cache hit and only get checked
  on a miss. Under `--frozen` or `--no-deps`, where nothing else would have
  fetched that document, turning on `--keyring` means paying one metadata
  request per collection that did not exist before.
- **A server's signatures are only as fresh as its cached metadata.** They
  travel only on the collection's version-detail document, which reaches
  verification for a dependency just as it does for a root, so a transitive
  dependency no requirements file names has its server's signatures checked
  too. For an
  exact version that document is read through the API cache, where it never
  expires and `--refresh` does not bypass it: once cached, a rerun or `warm`
  re-verifies against the same signatures without asking the server, and a
  signature a server adds or withdraws later is seen only under `--no-cache`
  or after `--clear-cache`.
- **Each declared network source gets one attempt.** An `http`/`https`
  source is fetched once, with no retry: one that never accepts the
  connection costs at most the 10-second dial timeout, and one that accepts
  and never answers costs `--timeout`. All of a collection's sources share
  the one 1-minute signature phase described under
  [install options](cli.md#install-options), and time rather than size is
  what spends it - 64 blobs at the 1 MiB maximum fit at about 1.1 MiB/s - so
  a raised `--timeout` lets a single unresponsive source spend the whole
  phase and leave the rest unfetched. A non-200 answer is reported by its
  status code alone and its body is never read; a 200 body past 1 MiB is
  refused, and none is sized from `Content-Length`. Blobs are gathered and
  checked one at a time, so a worker holds one in memory rather than up to
  64 MiB.
- **Nothing about a verification verdict is written anywhere** - not the
  snapshot, not the extract marker, not the lockfile, not `GALAXY.yml`. This
  project's trust model already treats the cache as attacker-writable, so a
  cached "already verified" is exactly the assertion this feature exists to
  refuse to take on faith. The consequence: an already-installed collection
  is skipped without being re-verified, the same gate that makes `install`'s
  "Up to date" line unaffected by verification at all, and the run says so
  once, on the result tier, naming how many collections were skipped and
  therefore left unverified this run - the answer to "I turned `--keyring`
  on over an existing workspace and nothing seemed to happen." `warm`
  carries no such gate: it re-verifies every collection it touches, cache
  hits included, so "Already warm" does not carry the same caveat "Up to
  date" does.
- **`--offline` warns about a signature source it cannot reach, once per
  run.** A `file://` source works offline, since it reads local state, but
  an `http`/`https` one does not; if some requirements root declares a
  network source, the run warns once, naming the first such root, rather
  than failing outright - a `file://` source listed first that verifies on
  its own means a later network source in the same list is never actually
  reached.
- **A dry run validates the setup and verifies nothing.** `install --dry-run`
  and `warm --dry-run` with a keyring configured print a caveat alongside the
  rest of the preview: the setup (keyring, required count) is echoed, but no
  signature source is fetched and no artifact's signatures are checked, so
  the preview's would-fail count never reflects a signature verdict.
- **The combined candidate set - declared sources plus whatever the server
  offers - is capped at 64 per collection**, with the declared sources
  always taking priority. If the combined set exceeds the cap, the run warns
  naming how many were dropped from the end. This is a different cap from
  the load-time one above: that one bounds what one requirements entry may
  *declare*, this one bounds what the *gather* actually reads once the
  server's own offer is added on top.
- **A failed verdict's own message renders at most 8 per-signature causes**,
  plus a footer naming which distinct failure statuses were left out. This
  bounds only the message: every cause stays reachable through `errors.Is`
  for exit-code purposes, and the cap is not a claim about how many
  signatures were checked - it is the count of distinct failure statuses a
  verdict from this tool can carry, so a message at the cap can still show
  every shape the vocabulary has.
- **A refused collection can still leave its artifact cached and its
  extracted tree populated**, since both are populated before the signature
  verdict is reached. This is the largest of a few disclosed residuals: the
  extracted store is content-addressed, so this is harmless there (an entry
  is only ever reachable by naming the sha of the bytes it holds), but the
  artifact cache slot is named by server and filename rather than by
  content, so a rejected artifact can occupy a name a later run's cache
  lookup serves without contacting the origin again - the same run repeating
  the check reaches the same refusal, and `--frozen` re-hashes against the
  lockfile pin regardless. A signature *verdict* on its own (the signatures
  in hand did not satisfy the policy) does not evict the cached artifact,
  since refetching would only re-read the same signatures again; a broken
  manifest chain, a missing `MANIFEST.json`, or a signature that vouches for
  a different collection all do evict and retry once.

A collection built from a git source carries no signature and can carry none:
its `MANIFEST.json` is written here, at build time, and nobody has signed it.
A verifying run therefore reports every git collection as the vacuous pass
described above, with the same warning, and a strict `+N` spelling fails it -
which is the honest answer, since the run was told to require a signature it
cannot have. Its attribution rests instead on the identity the builder read
from `galaxy.yml` being the one the resolve asked for, and on the manifest
chain check below, which the builder runs on every artifact it produces.

## Manifest chain check

The `MANIFEST.json` the signatures are checked over has to sit within the
first 64 MiB of the artifact's decompressed stream, and an empty one counts
as missing, since a detached signature over empty bytes verifies while
saying nothing about the artifact.

Once at least one signature verifies, the artifact is checked against
`MANIFEST.json` in both directions: every file, symlink and hardlink entry
the archive carries must be listed in `FILES.json` (a directory entry is
exempt from this side, since the check only ever records regular files,
symlinks and hardlinks even though extraction still creates it), and every
file `FILES.json` lists must hash to the digest it declares.
`MANIFEST.json` and `FILES.json` are both allowed to go unlisted by
`FILES.json` itself - a listing naming the documents that name it is a
convention, not a guarantee - but the exemption is asymmetric: a
`FILES.json` row naming itself with `ftype: file` is simply skipped, while a
`MANIFEST.json` row listed with a digest is checked like any other file and
can only ever fail, because the manifest's own bytes carry the listing's
digest, so no listing can name the manifest's digest without predicting
bytes that depend on it.

The check reads the archive a second time, apart from the manifest read,
and treats the tar stream as hostile even after a signature verified:
whoever controls the server or the cache can staple a legitimately signed
`MANIFEST.json` onto a tarball of their choosing, and the artifact's
declared sha256 comes from that same server. The chain proves only that an
archive agrees with a manifest, so it binds the two first: the archive's own
`MANIFEST.json` entry must be a regular file hashing to exactly the bytes
the signature was verified over. Each remaining rule closes a way an archive
could show the listing one thing and the extractor another:

- Two file or link entries whose cleaned paths collide are refused, with no
  exemption for `MANIFEST.json` or `FILES.json`: the manifest reader takes
  the first entry of a name, so a duplicate could show the signature one
  document and the chain walk another.
- A row with an `ftype` other than `file` over a path the archive carries as
  a regular file is refused, or that content would count as listed without
  ever being hashed. "Listed" means appearing in `files` at all, which is
  how a symlink to a directory, listed with `ftype: dir`, is covered.
- A symlink target that is absolute, or resolves to the archive root or
  above, is refused. A hardlink target is resolved against the archive root,
  as both the extractor and Python's `tarfile` read it. A listed link
  carries its target's content digest and is followed through at most 8
  links, which is also the cycle guard.
- An entry whose name normalizes to the archive root (`.`, `foo/..`) is
  skipped, as the extractor skips it.
- An unlisted entry is reported by the first one in stream order, so the
  message is the same on every run.

`FILES.json` itself is read more strictly than `encoding/json` would read
it, refusing shapes that another JSON reader, Python's `json.loads` among
them, could read differently: a top-level value that is not an object, a
`files` value that is not an array (`null` included), a second `files` key,
content after the top-level object, a name listed twice, a file row whose
`chksum_type` is not `sha256`, and a row that fails to decode, a type error
included. Any other key, such as `format`, is skipped, and a document with
no `files` key is an empty listing, so every entry the archive carries
besides `MANIFEST.json` and `FILES.json` is then refused as unlisted. A
listing is capped at 100,000 rows, the archive's own entry ceiling, and
every refusal of the listing's content, a listed name outside the archive
included, is a chain mismatch (exit `7`).

The signed manifest's declared `namespace`, `name` and `version` are also
checked against the collection this run actually resolved, byte for byte and
with no normalization, since a normalizing comparison is the one a
lookalike identity would be aimed at. A signature that verifies but names a
different collection (a downgrade, a substitution) fails the same way a
manifest whose identity cannot be read unambiguously does. The identity is
read key by key rather than decoded into a struct: `encoding/json` matches a
key case-insensitively and lets the last match win, while ansible-galaxy's
Python reader matches it exactly, so a `COLLECTION_INFO` shadowing
`collection_info`, or a `NAMESPACE` inside it, would declare one identity
here and another there. Exactly one key at each level may match the wanted
name case-insensitively, and it must be spelled exactly; a duplicate is
refused even when both carry the same value, and a value that is not a JSON
string is refused rather than coerced. The refusal quotes the declared
identity as one value, so a hostile version string cannot forge a line of
output. Like the chain check, this runs only once a signature verified,
since without one it would lend assurance to a document nobody vouched for.
See [Exit codes](exit-codes.md#exit-codes) for how each of these refusals
classifies.

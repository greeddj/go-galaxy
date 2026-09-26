# Security

How to check that a go-galaxy binary is the one its release workflow built,
and what a CI operator does to keep a run's trust boundaries intact.

## Verifying a release

Every release is signed, catalogued and attested by the workflow that built
it. cosign signs keylessly: the certificate's identity is the release workflow
itself, proved by a short-lived GitHub OIDC token, so there is no public key to
fetch and none to lose.

`checksums.txt` lists every asset by sha256 and is what gets signed, so one
signature and one hash cover whichever asset you downloaded:

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
registry beside the image, so nothing is downloaded first:

```bash
cosign verify ghcr.io/greeddj/go-galaxy:latest \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp \
    '^https://github\.com/greeddj/go-galaxy/\.github/workflows/release\.yml@refs/tags/'
```

A signature says the file is the one published. Provenance says which
workflow, at which commit, built it, and every asset has it as a GitHub build
attestation:

```bash
gh attestation verify go-galaxy-linux-amd64 --repo greeddj/go-galaxy
```

Every build also ships an SPDX SBOM of the Go modules linked into it, for a
vulnerability scan without unpacking anything. A raw binary is renamed for the
`releases/latest/download/` flow, but its SBOM keeps the build's name:

| Asset | Its SBOM |
| :-- | :-- |
| `go-galaxy_<version>_Linux_x86_64.tar.gz` | `go-galaxy_<version>_Linux_x86_64.tar.gz.sbom.json` |
| `go-galaxy-linux-amd64` | `go-galaxy_<version>_linux_amd64.sbom.json`; `go-galaxy-linux-amd64.sbom.json` does not exist |

### macOS and the Homebrew cask

macOS builds are not Apple-notarized, since that takes an Apple Developer ID
this project does not have, so Gatekeeper has nothing to check them against.
Verify the signature and provenance above instead.

A browser download arrives quarantined, and a quarantined copy of this binary
does not run. The Homebrew cask therefore runs `xattr -dr com.apple.quarantine`
on what it stages, in a post-install hook: a packager skipping a Gatekeeper
check for you. A cask without the hook would install a binary that never
starts.

Nothing else the release publishes touches the flag: raw binaries, archives
and images ship exactly as built. To keep Gatekeeper in the loop, install
another way and verify; the cask carries the archive's bytes, listed in the
same `checksums.txt`.

## Trust model

go-galaxy treats repository content (requirements files, `galaxy.toml`, the
lockfile), Galaxy servers and the cache as untrusted. What that leaves to you:

| Risk | What to do | Why |
| :-- | :-- | :-- |
| A secret on the command line | Use the variable in the table below | argv is readable by any local process for the whole run; go-galaxy cannot tell the routes apart to warn |
| A literal secret in `galaxy.toml` | Write `${VAR}` under `[tool.go-galaxy]` | A literal is the file's own credential, readable by anyone with the repository |
| A writable cache directory or S3 bucket | Restrict write access to the runs you trust | Cached metadata, download URLs and sha256 included, is served without asking the origin again |
| Untrusted branches or forks | Give them their own bucket, or a prefix plus a credential scoped to it, or no S3 cache | Any run against the bucket writes it, so a read-only credential cannot run at all |
| `--s3-prefix` taken as isolation | Scope the credential, not only the prefix | The prefix picks the keys a run uses, not the keys its credential may write, the lock included |
| A long run on a shared bucket | Keep untrusted runs off shared-bucket credentials | The lock is held for the whole run; a slow or hostile `source:` server locks other runners out |
| One cache, several Galaxy credentials | Give each credential its own cache directory, bucket or prefix | Entries are keyed by server, not token: a private answer is served to a later tokenless run |
| A poisoned cache | Install `--frozen` from a committed lockfile | Bytes must match the lockfile's sha256, or the run fails; an entry from a digest-less server has none |
| An artifact from another origin | Watch CI logs for the warning | It is warned, even under `--quiet`, not blocked, because separate content hosts are legitimate |
| A key in the keyring | Keep only keys you trust for every collection | Any trusted key vouches for any collection; no key is bound to a namespace |
| A revoked key | Refresh the keyring file | Revocation is only as fresh as that file: no keyserver or network check runs |
| The default signature policy | Write `+1` to require a signature | A bare count passes, with a warning, when nothing was gathered ([vacuous pass](signatures.md#required-count-and-the-vacuous-pass)) |
| An already-installed collection | Verify into an empty collections path | `install` skips it unverified and prints how many; `warm` re-verifies |
| A verdict cached as "verified" | Nothing: none is kept | No verdict is persisted anywhere, because the cache counts as attacker-writable |
| Archive metadata | Do not rely on modes, owners or mtimes | A signature covers file content through the manifest chain, not tar metadata |
| The dependency graph | Pin it with a lockfile | A signature covers the artifact; dependencies come from the Galaxy API |

| Flag | Put the value in |
| :-- | :-- |
| `--token` | `GO_GALAXY_TOKEN` |
| `--s3-secret-key` | `GO_GALAXY_S3_SECRET_KEY` or `AWS_SECRET_ACCESS_KEY` |
| `--s3-session-token` | `GO_GALAXY_S3_SESSION_TOKEN` or `AWS_SESSION_TOKEN` |

No token, or anything derived from one, is written to the snapshot, the project
registry, the lockfile, the metrics file or `GALAXY.yml`, and every
serialization prints it redacted. Text that go-galaxy did not generate has its
control characters replaced before it reaches your terminal or log.

## Residual risks

Two signature sources a requirements file may name are disclosed rather than
closed:

| Source | What it can do | What to do |
| :-- | :-- | :-- |
| `signatures:` `http(s)` URL | An extra outbound request; redirects are followed and no address class is refused, loopback and link-local included | Review `signatures:` changes like code; run untrusted branches where nothing private is reachable |
| `signatures:` `file://` path | Read a local absolute path the repository chose | Run untrusted branches as a user who can read nothing sensitive |

Either can leak through a message. A failed connection can name the resolved
address, the port and a certificate's valid names; a file that opens reaches
the verifier, whose errors can describe its content. The same grounds already
apply to the server addresses an `ansible.cfg` or `galaxy.toml` names.

Two more stay open. A verifying run opens the artifact three times, so a local
writer to the cache directory can swap bytes between passes: keep the cache
writable by the run's user only. A `file://` read has no deadline, so keep
signature files off network mounts.

How the code enforces each boundary is in
[Security boundaries](../internals/boundaries.md).

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
tag=v1.3.1
asset=go-galaxy-linux-amd64   # the file you install
base="https://github.com/greeddj/go-galaxy/releases/download/$tag"
curl -sSLf -O "$base/$asset" -O "$base/checksums.txt" \
  -O "$base/checksums.txt.sigstore.json"

cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp \
    '^https://github\.com/greeddj/go-galaxy/\.github/workflows/release\.yml@refs/tags/'

sha256sum --ignore-missing -c checksums.txt   # macOS: shasum -a 256 --ignore-missing -c checksums.txt
```

Run the last line where the asset keeps its release name: `--ignore-missing`
skips every asset that is not there. On macOS, use `shasum`: the system
`sha256sum` exits `0` having checked nothing when the asset is missing.

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

Homebrew quarantines what a cask downloads, as a browser does, and a
quarantined copy of this binary does not start. The cask therefore clears the
attribute in a post-install hook (`xattr -dr com.apple.quarantine`). That hook
skips a Gatekeeper check on your behalf. To check what the cask installs,
set `asset` above to its release archive, such as
`go-galaxy_1.3.1_Darwin_arm64.tar.gz`.

Nothing else the release publishes clears the attribute: raw binaries,
archives and images ship exactly as built. A `curl` download carries no
quarantine attribute and runs as is. A browser download does not start until
you clear the attribute. Verify the file as above, then run
`xattr -d com.apple.quarantine <file>`.

## Trust model

go-galaxy treats repository content (requirements files, `galaxy.toml`, the
lockfile), Galaxy servers and the cache as untrusted. The one exception is the
environment: a `galaxy.toml` is trusted with every variable its
`[tool.go-galaxy]` table names, and an `ansible.cfg`, as in ansible, with
every variable its install and cache paths name. Text that go-galaxy did not
generate has its control characters replaced before it reaches your terminal
or log. The tables below list what this model leaves to you.

### Secrets and the environment

| Risk | What to do | Why |
| :-- | :-- | :-- |
| A secret on the command line | Put it in the variable listed below | argv is visible to every local process for the whole run. go-galaxy cannot warn, because a flag and its variable look the same to it |
| A literal secret in `galaxy.toml` | Write `${VAR}` under `[tool.go-galaxy]` | Anyone who can read the repository can read and use it |
| A `galaxy.toml` you did not review | Export secrets only to runs whose `galaxy.toml` you trust | Its `[tool.go-galaxy]` table can expand any exported variable into a server `url` or `token`, or an S3 setting, and so send the value to a host the file picks |
| An `ansible.cfg` you did not review | Check its `collections_path`, `roles_path` and `cache_dir` before a run that exports secrets | Each expands `$VAR`, so the file can write a variable's value into a directory name and into the lines that print that path ([ansible.cfg paths](../reference/configuration.md#ansiblecfg-paths)) |
| A shared working directory, such as `/tmp` | Name your requirements file with `-r` | Without `-r`, go-galaxy reads a `./galaxy.toml` anyone can plant there in place of your `requirements.yml`, with only a warning. That file's `[tool.go-galaxy]` table then reads your environment |

Each secret flag has a variable:

| Flag | Put the value in |
| :-- | :-- |
| `--token` | `GO_GALAXY_TOKEN` |
| `--s3-secret-key` | `GO_GALAXY_S3_SECRET_KEY` or `AWS_SECRET_ACCESS_KEY` |
| `--s3-session-token` | `GO_GALAXY_S3_SESSION_TOKEN` or `AWS_SESSION_TOKEN` |

No token, or anything derived from one, is written to the snapshot, the
project registry, the lockfile, the metrics file or `GALAXY.yml`. Every
serialization prints it redacted.

### The cache and S3

| Risk | What to do | Why |
| :-- | :-- | :-- |
| A writable cache directory or S3 bucket | Restrict write access to the runs you trust | Cached metadata, download URLs and sha256 included, is served without asking the origin again |
| Untrusted branches or forks | Give them their own bucket, or no S3 cache. A prefix isolates them only when their credential can write under that prefix alone | Any run against the bucket writes it, the cache lock included, and holds that lock for the whole run. A read-only credential cannot run, and a slow or hostile `source:` server locks other runners out |
| One cache, several Galaxy credentials | Give each credential its own cache directory, bucket or prefix | Entries are keyed by server, not token. A private answer is then served to a later run without a token |
| A poisoned cache | Install `--frozen` from a committed lockfile | Galaxy and url collection bytes must match their sha256 in `galaxy.lock`, or the run fails ([What a frozen install checks](lockfile.md#what-a-frozen-install-checks)). A cached git collection, a cached role of any source and a collection from a server that publishes no sha256 install as cached. Restrict write access to the cache to protect them |
| An artifact from another origin | Watch CI logs for the warning | go-galaxy warns, even under `--quiet`, but does not block it: a separate content host is legitimate |

### Signatures and keys

| Risk | What to do | Why |
| :-- | :-- | :-- |
| A key in the keyring | Keep only keys you trust for every collection | Any key in the keyring vouches for any collection. No key is bound to a namespace |
| A revoked key | Refresh the keyring file | Revocation is only as fresh as that file. No keyserver or network check runs |
| The default signature policy | Write `+1` to require a signature, unless the project installs a git or url collection | A bare count passes, with a warning, when nothing was gathered. `+1` fails a git or url collection, which never has a signature ([Required count and the vacuous pass](signatures.md#required-count-and-the-vacuous-pass)) |
| An already-installed collection | Install into an empty collections path to check it | `install` skips it without re-hashing its files or verifying it, and a verifying run prints how many it skipped. `warm` verifies the cache, not the installed tree ([What a verifying run does differently](signatures.md#what-a-verifying-run-does-differently)) |

No "verified" verdict is kept anywhere, because the cache counts as
attacker-writable.

### What a signature does not cover

| Risk | What to do | Why |
| :-- | :-- | :-- |
| Archive metadata | Do not rely on modes, owners or mtimes | A signature covers file content through the manifest chain, not tar metadata |
| The dependency graph | Pin it with a lockfile | A signature covers the artifact. Dependencies come from the Galaxy API |

## Residual risks

These risks stay open. Each has a mitigation you apply:

| Risk | What it can do | What to do |
| :-- | :-- | :-- |
| A `signatures:` `http(s)` URL | Make an extra outbound request to any address, `localhost` and cloud metadata addresses such as `169.254.169.254` included. Redirects are followed | Review `signatures:` changes like code. Run untrusted branches where nothing private is reachable |
| A `signatures:` `file://` path | Read a local absolute path the repository chose | Run untrusted branches as a user who can read nothing sensitive |
| A local writer to the cache directory | Swap artifact bytes between the three reads of a verifying run | Keep the cache writable by the run's user only |
| A `file://` source on a network mount | Stall the run: the read has no deadline | Keep signature files on local disk |

A `signatures:` URL or `file://` path can also leak through a message. A
failed connection can name the resolved address, the port and a certificate's
valid names. A file that opens reaches the verifier, whose errors can describe
its content. Server addresses named in an `ansible.cfg` or `galaxy.toml` leak
the same way.

How the code enforces each boundary is on the internals page
[Security boundaries](../internals/boundaries.md).

# Signatures

`install` and `warm` can check each collection's detached OpenPGP signatures
against your keyring before extracting it. Verification is off until you set
`--keyring`.

> [!TIP]
> Signatures come from the Galaxy server and from any
> [`signatures:`](#signatures-in-the-requirements-file) you declare. Write `+1`
> to fail a collection that has none:
>
> ```sh
> gpg --export --armor KEYID > keyring.asc
> go-galaxy install --keyring keyring.asc --required-valid-signature-count +1
> ```
>
> A git or url collection never has a signature, so `+1` fails it. A project
> that installs one must keep a bare count, such as the default `1`.

```mermaid
flowchart LR
  A[Artifact downloaded or cached] --> B{Locked sha256 matches?}
  B -->|no| E7[Exit 7]
  B -->|yes or not locked| C["Gather declared, then server signatures"]
  C --> D{Count policy met?}
  D -->|no| E10[Exit 10]
  D -->|passed, none verified| W["Warn: nothing verified"]
  D -->|yes| F{Manifest chain and identity hold?}
  F -->|chain broken| E7
  F -->|another collection| E10
  F -->|yes| G[Extract]
  W --> G
```

## Turning it on

Set `--keyring` (`GO_GALAXY_KEYRING`, or ansible's `ANSIBLE_GALAXY_GPG_KEYRING`)
to switch verification on. The CLI reference lists the other signature options
under [Signatures](../reference/cli.md#signatures): the required count, the
ignored status codes and `--disable-gpg-verify`.

- Only `install` and `warm` read the signature options.
- A bare `~` or a leading `~/` in the keyring path expands to your home
  directory.
- An empty keyring or count, such as a withheld CI secret, exits
  [`2`](../reference/exit-codes.md) rather than falling back.
- `--disable-gpg-verify` beside a keyring warns that nothing is verified.
- `ANSIBLE_GALAXY_DISABLE_GPG_VERIFY` also accepts `yes`/`no` and `on`/`off`.

> [!WARNING]
> `ansible.cfg`'s `[galaxy]` signature keys are ignored with a warning, and
> `galaxy.toml` refuses them (exit `2`). A repository can ship either file, so
> use flags or variables.

## Keyring and signature file formats

```sh
cat teamA.asc teamB.asc > keyring.asc   # trusts both teams' keys
```

| Keyring file                                                   | Result    | Fix                                                                             |
|:---------------------------------------------------------------|:----------|:--------------------------------------------------------------------------------|
| `gpg --export --armor` output, or several concatenated         | accepted  | -                                                                               |
| Binary export or raw `pubring.gpg`, v3 certifications included | accepted  | -                                                                               |
| Exports glued with no newline                                  | exits `2` | End each with a newline                                                         |
| GnuPG keybox (`.kbx`)                                          | exits `2` | `gpg --no-default-keyring --keyring <kbx> --export --armor KEYID > keyring.asc` |
| Secret key material                                            | exits `2` | `gpg --export --armor KEYID > keyring.asc`                                      |
| No keys, or a non-key armor block                              | exits `2` | `gpg --export --armor KEYID > keyring.asc`                                      |

No `gpg` process runs: the format is judged from the bytes. Any key in the
keyring vouches for any collection, and revocation is only as fresh as the
keyring file ([Trust model](security.md#trust-model)).

<details markdown>
<summary>Packet rules</summary>

| File      | May hold                                                                                  | Refused                                        |
|:----------|:------------------------------------------------------------------------------------------|:-----------------------------------------------|
| Keyring   | Public keys and subkeys, user IDs and attributes, signatures, ring-trust packets, padding | Secret keys, marker packets, over 4096 packets |
| Signature | Signature packets, at most 64                                                             | Anything else                                  |

Each armor block costs one packet, so 1024 minimal three-packet exports load
and 1025 do not. The internals page
[Signatures and OpenPGP framing](../internals/boundaries.md#signatures-and-openpgp-framing)
gives the reasons.

</details>

## Required count and the vacuous pass

Without a `+` prefix, a collection with no signature passes with a warning.
That is the vacuous pass. Write `+1` to make it fail instead. The count applies
to every collection of the run, so `+1` also fails git and url collections,
which carry no signature. Roles are never verified.

| Spelling           | No signature gathered  | Otherwise passes when                        | Stops checking at |
|:-------------------|:-----------------------|:---------------------------------------------|:------------------|
| `1` (default), `N` | passes, warns          | N distinct keys verify                       | Nth key           |
| `+1`, `+N`         | fails                  | N distinct keys verify                       | Nth key           |
| `all`              | passes, warns          | no failure outside the ignore list           | never             |
| `+all`             | fails                  | one verifies, no failure outside ignore list | never             |
| `0`                | passes, warns          | nothing verifies, as in ansible              | never             |
| `+0`               | fails                  | never passes                                 | -                 |
| `-1`               | exits `2`: write `all` | -                                            | -                 |

A count of `N` stops at the Nth key that verifies, so a signature after it is
never checked. `0` does not make signatures optional. Under `0`, a collection
with a signature that verifies fails with exit `10`. To skip verification,
leave `--keyring` unset.

Keys are counted, not files: one key signing twice counts once. `ALL`, `++1`
and a count with spaces exit `2`.

## Ignored status codes

The ignore list changes the verdict only under `all` and `+all`. Under a
count, the number of keys that verify decides, and a bad signature never fails
a collection. There an ignored code only drops that failure from the error
message.

```sh
go-galaxy install --keyring keyring.asc --required-valid-signature-count +all \
  --ignore-signature-status-code NO_PUBKEY
```

| Code                                                                                  | Reported when                            |
|:--------------------------------------------------------------------------------------|:-----------------------------------------|
| `BADSIG`                                                                              | signature does not match `MANIFEST.json` |
| `NO_PUBKEY`                                                                           | signing key is not in the keyring        |
| `EXPKEYSIG`, alias `KEYEXPIRED`                                                       | signing key expired                      |
| `REVKEYSIG`, alias `KEYREVOKED`                                                       | signing key revoked                      |
| `EXPSIG`                                                                              | signature expired                        |
| `NODATA`                                                                              | blob or armor block is empty             |
| `BADARMOR`                                                                            | armor cannot be decoded                  |
| `ERRSIG`                                                                              | malformed packets, or any other error    |
| `MISSING_PASSPHRASE`, `BAD_PASSPHRASE`, `NO_SECKEY`, `UNEXPECTED`, `ERROR`, `FAILURE` | never, so ignoring one has no effect     |

The flag repeats, and its variables take a comma-separated list. Values are
trimmed and case-insensitive. Any other code, or an empty element, exits `2`.

## Signatures in the requirements file

=== "galaxy.toml"

    ```toml
    [[project.collections]]
    name = "community.general"
    version = "11.1.0"
    signatures = [
      "https://sigs.example.com/community.general-11.1.0.asc",
      "file:///etc/pki/collections/community.general.asc",
    ]
    ```

=== "requirements.yml"

    ```yaml
    collections:
      - name: community.general
        version: "11.1.0"
        signatures:
          - https://sigs.example.com/community.general-11.1.0.asc
          - file:///etc/pki/collections/community.general.asc
    ```

| Source                                            | Accepted |
|:--------------------------------------------------|:---------|
| `https://...` or `http://...` with a host         | yes      |
| `file:///abs/path` or `file://localhost/abs/path` | yes      |
| relative path, other scheme, `file://otherhost/`  | no       |
| URL carrying userinfo, such as `user@`            | no       |
| over 64 sources per collection                    | no       |

`signatures:` takes one source or a list. A refused source exits `2` when the
file loads, before any request. Git, url and role entries refuse the key
([What is refused](requirements.md#what-is-refused)). A run with
`signatures:` but no keyring exits `2`. Under `--disable-gpg-verify` it only
warns.

A source is fetched with no Galaxy token and no relaxed TLS, so a private CA
must be trusted system-wide
([TLS and a private CA](servers-and-auth.md#tls-and-a-private-ca)). Its query
string is sent but never printed or stored.

## What a verifying run does differently

| Situation                                     | What happens                                                                                                                                                                                                             |
|:----------------------------------------------|:-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `warm`                                        | Verifies every collection, cache hits included                                                                                                                                                                           |
| Every Galaxy collection, cached or `--frozen` | Reads its version metadata (API cache first) for server signatures                                                                                                                                                       |
| Server adds or withdraws a signature          | Seen under `--no-cache`, after `--clear-cache`, or once the cached version metadata is dropped 30 days after it was written ([Freshness and retention](caching.md#freshness-and-retention)). `--refresh` does not see it |
| Unfetchable declared source                   | Fails the collection when reached (exit `5`), with no retry                                                                                                                                                              |
| `--offline`                                   | `file://` sources work. A network source warns, and fails the collection if reached. Server signatures come only from cached metadata, so warm with `--keyring` first                                                    |
| `--dry-run`                                   | Checks the setup and fetches no signature                                                                                                                                                                                |
| Chain or identity fails on a cache hit        | Downloads once more, unless `--offline`                                                                                                                                                                                  |
| Signature policy fails on a cache hit         | Not refetched: each run that reads the cache fails the same way until `--clear-cache`                                                                                                                                    |

> [!WARNING]
> `install` skips an installed collection unverified and prints how many.
> Install into an empty collections path to verify what playbooks load.
> `warm` checks only the cache.

<details markdown>
<summary>Limits</summary>

| Limit                              | Value                      |
|:-----------------------------------|:---------------------------|
| Signatures gathered per collection | 64, declared sources first |
| One signature                      | 1 MiB                      |
| Keyring                            | 64 MiB, 4096 packets       |

Connection and time budgets are in
[Timeouts and fixed limits](../reference/cli.md#timeouts-and-fixed-limits).

</details>

## Manifest chain check

A verifying run reads each collection's `MANIFEST.json` first, signed or not.
An artifact with no `MANIFEST.json`, or an empty one, in its first 64 MiB
exits `5`.

```mermaid
flowchart LR
  S[Verified signature] -->|covers| M[MANIFEST.json]
  M -->|names digest of| F[FILES.json]
  F -->|lists sha256 of| H[Every archive file]
  M -->|declares| I["Namespace, name, version"]
  I -->|must equal| R[Resolved collection]
```

The chain check runs once a signature verifies. It covers every file, symlink
and hardlink in the archive. Mixed failures follow
[When several things fail](../reference/exit-codes.md#when-several-things-fail).

| Refusal                                                              | Exit |
|:---------------------------------------------------------------------|:-----|
| Unlisted entry, wrong digest, or `MANIFEST.json` not the signed copy | `7`  |
| Manifest names another collection, or its identity is ambiguous      | `10` |

<details markdown>
<summary>What the chain check refuses</summary>

| Archive or listing                                                                     | Exit |
|:---------------------------------------------------------------------------------------|:-----|
| Two entries with one cleaned path                                                      | `5`  |
| A symlink that is absolute or resolves to the archive root or above                    | `5`  |
| A listed `ftype` other than `file` over a regular file                                 | `7`  |
| A link followed through more than 8 hops                                               | `7`  |
| `FILES.json` with a second `files` key, a name listed twice or a non-`sha256` checksum | `7`  |
| A listing over 100,000 rows                                                            | `7`  |

`MANIFEST.json` and `FILES.json` may go unlisted. Each identity key must appear
once, spelled exactly, and match byte for byte. The internals page
[Reading a manifest](../internals/boundaries.md#reading-a-manifest) gives the
reasons.

</details>

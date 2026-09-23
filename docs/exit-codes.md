# Exit codes

`go-galaxy` exits with a class-specific code instead of a flat `1`, so CI
pipelines can branch on failure type without parsing log output. The failure
classes and their one-phrase meanings are printed by `go-galaxy --help` as an
index, `130` among them; the other two signal codes and every qualification
below are the part only this table carries. A caught signal follows the shell convention of
`128 + signal number`, so the tool's own classes and its signal codes can never
collide:

| Code | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
|-----:|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------                                                                                                                         |
|    0 | Success                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
|    1 | Generic failure (does not match any class below)                                                                                                                                                                                                                                                                                                                                                                                                                  |
|    2 | Usage or configuration error (invalid flags, a positional argument missing where a command requires one (`explain` with no name) or given where it takes none - a word that names no command included, since it reaches `install` as one - requirements, `ansible.cfg`, an unsupported collection source (`file`, `dir`), a git source this tool refuses as written - a malformed or credential-bearing URL, an abbreviated or invalid ref, an unsafe subdir, a credential binding that does not parse, an ssh repository with neither a bound key nor an agent, a repository or subdir holding no collection or naming one the requirement did not ask for, a `galaxy.yml` that cannot be built from or whose version is not exact - a url source this tool refuses as written - a malformed, credential-bearing or fragment-bearing URL, a non-exact `version:` assertion, a `source:`, `namespace:` or `signatures:` key on a url entry, a url credential binding that does not parse, a role tarball that does not hold exactly one role - an explicit namespace conflicting with a dotted collection name, a `roles:` entry this tool refuses as written - not a list, a shape ansible would not take either, a collection key (`source:`, `signatures:`, `type:`) on a role entry, a name, install name or version outside the role alphabets, a local-path, non-http or non-`.tar.gz` `src:`, an `scm` other than `git`, an `include:`, or two entries installing into one directory - no configured Galaxy server serving the v1 role API, a v1 role record this tool cannot compose a repository URL from, a repository that is not a role (no `meta/main.yml` at its root) or whose meta this tool cannot read, an unsupported cache-snapshot schema version, an unreadable or unparseable project requirements file, or a cache backend that cannot be used as configured). The Go runtime also exits a fatal error - a stack overflow, an out-of-memory kill - with this same status, without running any cleanup; a run that ended that way prints a line beginning `fatal error:` to stderr, so it is the output rather than the status that tells the two apart |
|    3 | Dependency resolution failure (conflicts, missing candidates, cycle, a git ref or commit the remote does not have, a Galaxy role no v1-serving server knows, a role version the server does not list, or a role whose version names cannot be ordered so no highest one can be chosen)                                                                                                                                                                                                                                                                                                                                                                                              |
|    4 | Network or Galaxy API failure (timeouts, stalled transfers, metadata and cache-state deadlines, a git transport failure or a git credential or host key the remote refused, a git pack that exceeded its on-disk ceiling, a Galaxy metadata URL no HTTP request can be built from, a versions listing that exceeded its page ceiling (a collection's, or a role's v1 version list past 20 pages), a response body that exceeded its size ceiling (an artifact, a metadata document, or a bucket listing), a v1 role API request that failed in transport or with a 5xx, offline-mode violations (a role with no recorded pin or no cached artifact under `--offline` included), an unreachable cache backend, or an `outdated` run in which at least one latest-version lookup failed for a reason not classified below - a lookup that failed on a userinfo refusal reports `5`)                                                                        |
|    5 | Install-time failure (unsafe archive/symlink content, empty file, missing artifact cache, a git tree this tool will not materialize - an unsafe entry name, case-folded duplicates, a symlink that resolves nowhere - or a git artifact that failed its own build self-check, a role directory that already exists and was installed neither by this tool nor by `ansible-galaxy` so it is not replaced, a role artifact that failed to extract, or a URL a Galaxy server supplied that this tool refuses to fetch from because it embeds a credential in its userinfo - an artifact download URL or a metadata URL alike, so a `lock` or `outdated` run can report `5` without ever installing anything)                                                                                                                |
|    6 | Lockfile error (missing, invalid, mismatched with requirements, or out of date under `lock --frozen`; for a role, a `roles:` entry the lockfile lacks or locks from a different Galaxy version, repository or ref, a role entry whose fields are not canonical, or a role entry in a file whose `schema_version` is below 3)                                                                                                                                                                                                                                                                                                                                                             |
|    7 | Artifact-integrity failure (content does not authenticate against its naming sha256, or the digest is malformed; for a git source, a remote that advertised one commit and shipped another, or a pinned commit that no longer builds the collection it was pinned as; for a role, a repository that serves a different commit than the lockfile pins when the artifact has to be rebuilt)                                                                                                                                                                                                                                                                                                                                                  |
|    8 | Cache contention (the cache lock is held elsewhere, the S3 lock's wait ceiling elapsed after this run observed another holder, or a lock this run did hold was taken away by another holder mid-run)                                                                                                                                                                                                                                                              |
|    9 | Persisted cache state is corrupt or oversized and must be discarded (a project registry that fails to decode, a state object that exceeds its size ceiling or cannot be read back as what go-galaxy writes there, or - local backend only - a Bolt snapshot file that fails one of its own corruption checks)                                                                                                                                                                                                           |
|   10 | Signature verification failure - this run failed to attribute a collection's artifact to a publisher it was configured to accept: either its signatures did not satisfy the policy in force (fewer valid than required, or a failure under an `all` policy), or they did and vouched for a different collection, or its manifest's declared identity could not be read unambiguously. The artifact's own bytes are a separate question and stay exit `7`          |
|  129 | Interrupted (a caught SIGHUP)                                                                                                                                                                                                                                                                                                                                                                                                                                     |
|  130 | Interrupted (a caught SIGINT, or the caller's own context canceled)                                                                                                                                                                                                                                                                                                                                                                                               |
|  143 | Interrupted (a caught SIGTERM)                                                                                                                                                                                                                                                                                                                                                                                                                                    |

Exit `6`'s "missing" half is uniform across every command that requires a
lockfile: `install --frozen`, `warm --frozen`, `lock --frozen`, `tree` and
`explain` all exit `6` when the lockfile they were told to read is not there,
rather than treating its absence as a usage error. A role adds no exit code of
its own: every role failure classifies into the classes above by what failed,
so a pipeline branching on these numbers needs no new branch.

Two commands read somewhere else instead of requiring the file, and neither
adds an exit class for doing so. `hash` exits `0` and falls back to hashing
`requirements.yml`. `outdated` falls back to the installed collections tree
and reports from it; it still exits `6` when that tree is missing too, naming
both paths, so a repository that neither locks nor installs is told the same
thing it always was.

Exit `7` covers content that failed to authenticate against the sha256 that
named it - a lockfile pin, a Galaxy server's declared digest, a cache sidecar,
or the extracted store's content-address key - or a digest that was
structurally malformed. It is a stop-and-alert class: do not retry it
automatically. A retry cannot repair it, because the bytes or the digest are
wrong at the source, not transiently unavailable. It also outranks the network
class: a run that hits both an integrity failure and a network failure exits
`7`, not `4`.

Exit `8`'s lock-loss half outranks exit `7` in turn: a run that both lost the
cache lock mid-run and failed an integrity check exits `8`. Once another holder
is writing the same cache, this run's own checksum verdict is no longer
evidence about the artifact - it may simply be that other holder rewriting the
artifact underneath it - so the exclusivity failure is the actionable fact and
the mismatch is a symptom of it. Fix the contention first, then rerun; if the
integrity failure is real, the rerun reports it as exit `7` with nothing else
touching the cache.

The same holds against every other class: once another holder took the lock,
whatever the run returned is reported as exit `8`, a run that otherwise
succeeded included, since the heartbeat notices the loss only after some work
was already done without exclusivity. The backend stops the rest of the run by
canceling it, and that cancellation exits `8` too, never `130`; only a caught
signal or the caller's own cancellation outranks a lost lock.

A run in which individual collections or roles fail prints each failure live
as a `Failed:` line and ends with one headline error -
`installation failed for N collections`, or `installation failed: warm failed
for N collections` for `warm` - with every recorded cause kept behind it. The
exit code is the first class, in the order
[Exit code classes](commands.md#exit-code-classes) draws, that matches the
headline or any cause. The headline is an install failure, so only a cause
ranked above that class changes the code: an interrupt exits `130`, an
integrity failure `7`, a signature verdict `10` and a lockfile error `6`,
which is why a `--frozen`
install whose sha256 pin does not match the artifact exits `7`, not `5`. Every
other cause leaves the run at `5`, even one that alone would exit elsewhere: a
network failure, or a collection not cached under `--offline`, exits `4` only
where it ends the run by itself, outside the per-collection path. `outdated`
follows the same rule behind its own headline, `latest version lookup failed
for N collections`, whose class is `4`.

The snapshot save at the end of `install`, `warm` and `lock` never replaces the
run's own failure. When collections or roles also failed, or `lock --frozen`
found drift, the save error is appended to that failure's message after
`snapshot save failed:` and the run keeps the class the failure decides, `6`
for the drift; the save error decides the exit code only when nothing else
failed, as `cache backend unavailable` exiting `4` does. A plain `lock` writes
the lockfile, and prints `Lockfile written`, before it saves the snapshot, so a
`lock` that exits nonzero on a failed save still leaves a valid new lockfile on
disk; `lock --frozen` never writes one.

A stalled or byte-dripped transfer is never reported as an interrupt, even
though the underlying mechanism that unblocks it is a context cancellation:
the tool distinguishes its own no-progress cancellation from a genuine caught
signal or caller cancellation, and only the latter exits `130` (SIGINT or a
canceled caller context), `143` (SIGTERM) or `129` (SIGHUP). SIGTERM is the one
worth planning for: a canceled GitLab job, an evicted Kubernetes pod and a
canceled GitHub Actions job all send it rather than SIGINT, so `143` is the
code a cancellation usually shows up as. SIGQUIT is deliberately left
unhandled, which keeps Go's default goroutine dump available for diagnosing a
hung run. The same holds for
the metadata, cache-state and signature-fetch ceilings above: grep the run's
output for `galaxy metadata fetch deadline exceeded`, `cache state object
deadline exceeded`, or `collection signature fetch deadline exceeded` to tell
one of these deadlines apart from a genuine interrupt or from any other
network failure sharing exit code `4`.

One signature-related failure is a deliberate exception to that rule rather
than a fourth deadline: a signature source that could not be fetched or read
(reported as `collection signature source unavailable`, distinct from the
deadline message above) keeps a genuine Ctrl-C reachable, so an operator
interrupting a run mid-fetch of a signature source still exits
`130`/`143`/`129` as an interrupt, not `4` as a network failure. That is the
one place in this whole family where the underlying transport error is
allowed to carry the caller's own cancellation through unchanged, because
unlike the four deadlines above, this failure already reports every other
network cause faithfully - a stall here is the deadline sentinel's own job,
not this one's - so there is nothing for a real Ctrl-C to be confused with.

`artifact download deadline exceeded` is the stall verdict for every
acquisition that spends the artifact budget described under
[install options](cli.md#install-options) - a Galaxy or url artifact, a git
fetch, a role - and two verdicts keep their own class even when that budget ran
out at the same moment. A sha256 mismatch keeps exit `7`: a digest is compared
only after a complete copy, so a transfer the deadline cut short never yields a
false mismatch. A cache backend that cannot be used as configured keeps exit
`2`, so a remote cannot turn a configuration error into a retryable exit `4` by
stalling until the budget runs out.

Exit codes `2`, `4`, and `8` each fold in more than one cache-backend
condition too, distinguishable the same way - grep the run's output for the
message: `cache backend cannot be used as configured` (exit `2` - an
`--s3-endpoint` that parses to no host, a bucket that does not enforce
create-if-absent or compare-and-swap so the distributed lock cannot work, or -
on the local backend, with no S3 involved at all - any permission failure
against the cache directory, which is the wrong-uid case the [container image
bake](ci.md#container-image-bake) describes),
`cache backend unavailable` (exit `4` - the backend could not be reached, or
answered with a failure that is not this program's own doing), `another
process holds the cache` (exit `8` - a local Bolt file open timed out against
another process's held lock, or the S3 lock's wait ceiling elapsed after this
run observed another acquirer holding it), `another instance is running`
(exit `8` - a second local run found the lock already held and refused to
start immediately), and `cache lock ownership was lost to another holder`
(exit `8` - this run acquired the lock and a later heartbeat found another
acquirer's token on it, so the work it had already done was not exclusive).

On the S3 backend, exit `8` reached through the wait means this run saw another
acquirer holding the cache lock at some point during the wait - not that the
backend was still healthy when the wait gave up, since one observation early in
the wait is enough even if the backend answers nothing at all for the rest of
it. A run that never got such an answer from the bucket in the first place -
one that accepts connections and never replies, replies only with failures, or
contradicts itself about whether the lock object exists - exits `4` with
`cache backend unavailable` instead. The `cache lock ownership was lost to
another holder` half has the opposite shape and is not covered by that
sentence: there was no wait at all, the lock was granted, and the positive
observation of another holder came afterward, from a heartbeat during the run.

Exit `9` means the persisted cache state itself - not this reader's ability
to interpret it - cannot be trusted by anyone and must be discarded before the
run can proceed: grep the run's output for `corrupt project registry`,
`cache state object exceeds the maximum allowed size`, `corrupt cache state
object`, or `corrupt snapshot store` to tell which one fired. The last of the
four is local-backend only: it means the local cache directory's Bolt snapshot
file itself failed one of its own corruption checks, not merely that this run's
own reader could not make sense of it. Only bbolt's own corruption checks count,
as [What the directory holds](caching.md#what-the-directory-holds) lists them;
a permission failure opening the file exits `2` and an mmap failure `4`, with
the cache-backend messages above, since discarding a healthy cache would fix
neither. The remedy is mechanical and safe to automate: delete the offending object (or the whole cache directory / bucket
prefix), or rerun with `--clear-cache`, then rerun the command. This is
deliberately distinct from exit `2`: a snapshot a newer binary wrote in a
schema this one cannot safely interpret (`unsupported snapshot schema
version`) exits `2` instead, since the snapshot itself is not damaged, only
unreadable by this particular binary, and discarding it would destroy a shared
cache other, newer runners still depend on.

Exit `10` means this run failed to attribute a collection's artifact to a
publisher it was configured to accept, not that its content is wrong: an
artifact can hash exactly as its digest says and still exit `10`. That
failure to attribute can happen in either direction - the signatures in hand
did not satisfy the policy in force, or they satisfied it and vouched for a
different collection than the one being installed (MANIFEST.json's own
namespace, name and version disagreeing with what was resolved, or a
manifest whose identity cannot be read unambiguously at all, such as two
conflicting spellings of the same JSON key) - and both share the same class
rather than part of exit `7`, because both leave an operator holding bytes
nobody they trust vouched for. The remedy is usually this run's own keyring
or its required-signature-count policy, not the artifact or the server that
served it. Every other signature-related failure classifies by what actually
failed rather than by the phase it was found in: a keyring that cannot be
read, a keyring in a container format this tool cannot open, `signatures:`
declared with no keyring configured, an unaccepted required-count,
ignored-status-code, or disable-verification value, an explicitly supplied
but empty `--keyring` or `--required-valid-signature-count`, a requirements
entry declaring more signature sources than this tool gathers for one
collection (64), and a signature source this tool would never fetch all exit
`2`. That last class is about the source's own spelling rather than about
reaching it: a value naming nothing fetchable (no scheme, a scheme outside
`file`, `http` and `https`, an `http`/`https` URL naming no host such as
`https:///sig.asc`, or a `file` URL naming another host or a relative path),
or one embedding a credential in its userinfo
(`https://user:pass@hub/sig.asc`), is refused before any request is
composed, and the remedy is editing the entry in `requirements.yml`. A source
this tool would have fetched and could not obtain - a network failure, an
unreadable `file://` path, an offline-mode refusal - or a signature phase that
overran its deadline, exits `4` instead; an artifact carrying no
`MANIFEST.json` exits `5` with the other artifact-shape failures; and a
manifest chain that does not match its own digests exits `7`, since what failed
there is bytes against a digest.

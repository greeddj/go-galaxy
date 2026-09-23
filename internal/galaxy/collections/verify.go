package collections

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/psvmcc/hub/pkg/types"
)

// serverSignatureField is the one key of a Galaxy version-metadata signature
// entry this tool reads; a fingerprint or signing service beside it decides
// nothing, since the keyring decides which key counts.
const serverSignatureField = "signature"

// verifyContext is one run's signature verification state; a nil *verifyContext
// means verification is off. It is built once and read by every worker without
// a lock, so nothing but skippedUnverified may be written after construction.
type verifyContext struct {
	keyring *signature.Keyring
	fetcher *signature.Fetcher
	// sources maps "<namespace>.<name>" to that entry's declared signature
	// sources, deduped, in file order; a collection declaring none is absent.
	sources map[string][]string
	policy  signature.Policy
	// skippedUnverified counts collections skipped as already installed, hence
	// unverified; atomic because the install workers write it concurrently.
	skippedUnverified atomic.Int64
}

// recordSkippedUnverified counts one collection skipped as already installed. It
// is nil-receiver-safe, so the install worker's call site needs no branch for a
// run that verifies nothing - such a run has nothing to report either way.
func (vc *verifyContext) recordSkippedUnverified() {
	if vc == nil {
		return
	}
	vc.skippedUnverified.Add(1)
}

// reportSkippedUnverified prints once, on the result tier, how many
// already-installed collections this verifying run left unverified: the skip
// lines themselves are transient, and --quiet or a non-TTY CI drops them.
func (vc *verifyContext) reportSkippedUnverified(runtime *infra.Infra) {
	if !vc.enabled() {
		return
	}
	skipped := vc.skippedUnverified.Load()
	if skipped == 0 {
		return
	}
	runtime.Output.PersistentPrintf(
		"🔏 %d already-installed collection(s) were skipped and therefore not verified on this run", skipped)
}

// newVerifyContext resolves this run's verification state, returning (nil, nil)
// when nothing is verified, which reads no keyring and builds no client. It is
// the one funnel install and warm share, so the keyring and Fetcher are per run.
func newVerifyContext(cfg *config.Config, runtime *infra.Infra, roots []collection) (*verifyContext, error) {
	// Emitted before the enabled check, so the operator whose keyring sits in
	// an ansible.cfg - the shape this warning exists for - is told about it on
	// the very run that verifies nothing because of it.
	if warning := cfg.AnsibleSignatureKeysWarning(); warning != "" {
		runtime.Output.Warnf("%s", warning)
	}
	if !signature.VerificationEnabled(cfg.Signature.KeyringPath, cfg.Signature.DisableGPGVerify) {
		return nil, verificationOffError(runtime, cfg.Signature, roots)
	}

	policy, err := signature.NewPolicy(
		cfg.Signature.KeyringPath, cfg.Signature.RequiredCount, cfg.Signature.IgnoreStatusCodes, cfg.Signature.DisableGPGVerify)
	if err != nil {
		return nil, err
	}
	keyring, err := signature.LoadKeyring(policy.KeyringPath)
	if err != nil {
		return nil, err
	}
	announceVerification(cfg, runtime, keyring)
	// Only once verification is known to be on: a run that verifies nothing
	// fetches nothing, so the offline warning would be noise there.
	warnOfflineSignatureSources(cfg, runtime, roots)

	return &verifyContext{
		keyring: keyring,
		fetcher: signature.NewFetcher(cfg.Timeout, cfg.Offline),
		sources: requirementSources(roots),
		policy:  policy,
	}, nil
}

// announceVerification states the keyring and required count once per run: a
// result-tier line on a real run, and on a dry run a stderr caveat that the
// preview verifies nothing, true because classifyDryRun has no verify branch.
func announceVerification(cfg *config.Config, runtime *infra.Infra, keyring *signature.Keyring) {
	if cfg.DryRun {
		runtime.Output.Warnf(
			"signature verification is configured and this preview validates the setup and verifies no collection: "+
				"keyring %s, required count %s; no signature source is fetched and no artifact's signatures are checked, "+
				"so the would-fail count covers no signature verdict",
			keyring.Path(), cfg.Signature.RequiredCount)

		return
	}
	runtime.Output.PersistentPrintf("Signature verification on: keyring %s, required count %s",
		keyring.Path(), cfg.Signature.RequiredCount)
}

// warnOfflineSignatureSources warns once when --offline meets a declared network
// signature source, naming the root, never the source URI. A warning, not a
// refusal: an earlier file:// source may satisfy the policy on its own.
func warnOfflineSignatureSources(cfg *config.Config, runtime *infra.Infra, roots []collection) {
	if !cfg.Offline {
		return
	}
	for _, root := range roots {
		for _, source := range root.Signatures {
			if !signature.SourceRequiresNetwork(source) {
				continue
			}
			runtime.Output.Warnf(
				"--offline: %s declares a signature source that must be fetched over the network, which offline mode "+
					"refuses; a collection whose gather reaches one fails rather than installing unverified - "+
					"use a file:// source, or drop --offline",
				requirementKey(root))

			return
		}
	}
}

// verificationOffError refuses, as ansible-galaxy does, a run that declares
// signatures with no keyring configured; under --disable-gpg-verify it only
// warns, since helpers.ErrKeyringRequired would misstate the configuration.
func verificationOffError(runtime *infra.Infra, sc config.SignatureConfig, roots []collection) error {
	i := slices.IndexFunc(roots, func(root collection) bool { return len(root.Signatures) > 0 })
	if i < 0 {
		return nil
	}
	named := requirementKey(roots[i])
	if sc.DisableGPGVerify {
		runtime.Output.Warnf(
			"signature verification is disabled; the signature sources %s declares - and any other collection's - will not be checked",
			named)
		return nil
	}

	return fmt.Errorf("%w: %s declares signature sources; configure a keyring with --keyring "+
		"(or GO_GALAXY_KEYRING), or drop the signatures: block", helpers.ErrKeyringRequired, named)
}

// requirementSources collects each root's declared signature sources by
// requirementKey, deduped and in file order, which is the gather's order and so
// the order a required count is reached in.
func requirementSources(roots []collection) map[string][]string {
	sources := make(map[string][]string)
	for _, root := range roots {
		if len(root.Signatures) == 0 {
			continue
		}
		key := requirementKey(root)
		for _, source := range root.Signatures {
			if !slices.Contains(sources[key], source) {
				sources[key] = append(sources[key], source)
			}
		}
	}

	return sources
}

// requirementKey is the "<namespace>.<name>" a requirements entry is addressed
// by, which is what a root's signature sources are keyed under: a root names
// the collection, never a particular version of it.
func requirementKey(col collection) string {
	return col.Namespace + "." + col.Name
}

// enabled reports whether this run verifies signatures, and is safe on a nil
// receiver - which is what a run that verifies nothing carries, so every call
// site is one method call rather than a nil check plus a field read.
func (vc *verifyContext) enabled() bool {
	return vc != nil
}

// verifyCollectionSignatures checks col's detached signatures over MANIFEST.json
// and, once one verified, the manifest chain and attribution. The bytes must come
// from manifest.ReadFromTarGz, the one place their size bound is applied.
func verifyCollectionSignatures(ctx context.Context, deps installDeps, col collection, payload installPayload) error {
	vc := deps.verify
	if !vc.enabled() {
		return nil
	}
	tarPath := payload.artifact.Path
	manifestBytes, err := manifest.ReadFromTarGz(ctx, tarPath)
	if err != nil {
		return err
	}

	// The budget covers the whole gather, interleaved key checks included; the
	// chain walk below does no network I/O and so runs under ctx instead.
	budget := deps.runtime.SignatureDeadline()
	sigCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	result, err := signature.Verify(manifestBytes, vc.nextBlob(sigCtx, deps.runtime, col, payload.meta), vc.keyring, vc.policy)
	if err != nil {
		return fmt.Errorf("%s: %w", col.key(), signatureDeadlineError(ctx, sigCtx, budget, err))
	}
	if result.VacuousPass {
		warnVacuousPass(deps.runtime, col, vc.policy.Required, payload.metaUnavailable)
	}
	if result.Verified == 0 {
		return nil
	}
	if err := manifest.VerifyChain(ctx, tarPath, manifestBytes); err != nil {
		return fmt.Errorf("%s: %w", col.key(), err)
	}
	if err := checkManifestAttribution(col, manifestBytes); err != nil {
		return fmt.Errorf("%s: %w", col.key(), err)
	}
	deps.runtime.Output.Debugf("Verified %s against %d key(s)", col.key(), result.Verified)

	return nil
}

// warnVacuousPass warns that the policy passed with nothing verified, telling
// apart version metadata that was unavailable (so never gathered) from none
// offered, and names the strict count spelling that would make it fatal.
func warnVacuousPass(runtime *infra.Infra, col collection, required signature.CountSpec, metaUnavailable bool) {
	if metaUnavailable {
		runtime.Output.Warnf(
			"Nothing verified %s and the policy passed anyway; its version metadata was unavailable, "+
				"so any signatures the server carries were never gathered - write %q to require that at least one verified",
			col.key(), strictSpelling(required))

		return
	}
	runtime.Output.Warnf(
		"Nothing verified %s and the policy passed anyway; write %q to require that at least one verified",
		col.key(), strictSpelling(required))
}

// The MANIFEST.json identity keys checkManifestAttribution reads, as literals
// rather than struct tags because readIdentityObject judges the raw key bytes.
const (
	manifestCollectionInfoKey = "collection_info"
	manifestNamespaceKey      = "namespace"
	manifestNameKey           = "name"
	manifestVersionKey        = "version"
)

// checkManifestAttribution requires the signed manifest to declare, byte for
// byte, the namespace, name and version this run resolved, so a signature cannot
// vouch for a substitute or a downgrade; it runs only once a signature verified.
func checkManifestAttribution(col collection, manifestJSON []byte) error {
	info, err := readIdentityObject(manifestJSON, manifestCollectionInfoKey)
	if err != nil {
		return err
	}
	namespace, err := readIdentityField(info, manifestNamespaceKey)
	if err != nil {
		return err
	}
	name, err := readIdentityField(info, manifestNameKey)
	if err != nil {
		return err
	}
	version, err := readIdentityField(info, manifestVersionKey)
	if err != nil {
		return err
	}
	if namespace == col.Namespace && name == col.Name && version == col.Version {
		return nil
	}

	// Each archive-chosen component is bounded alone and the three are quoted as
	// one %q value, since internal/safeout passes a newline through unescaped.
	declared := helpers.TruncateForMessage(namespace) + "." +
		helpers.TruncateForMessage(name) + "@" + helpers.TruncateForMessage(version)

	return fmt.Errorf("%w: %s vouches for %q",
		helpers.ErrSignatureAttributionMismatch, helpers.ManifestFileName, declared)
}

// readIdentityObject returns the value of the one key spelled exactly want, and
// refuses a document where any other key case-folds to it: a struct decode folds
// and keeps the last match, so parsers would disagree on the declared identity.
func readIdentityObject(raw []byte, want string) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, unreadableIdentity(want, "the document is not a JSON object")
	}

	var found json.RawMessage
	matches := 0
	for dec.More() {
		key, ok := identityKey(dec)
		if !ok {
			return nil, unreadableIdentity(want, "its keys do not read as a JSON object's")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, unreadableIdentity(want, "one of its values does not parse")
		}
		if !strings.EqualFold(key, want) {
			continue
		}
		matches++
		if key == want {
			found = value
		}
	}
	if matches != 1 || found == nil {
		return nil, unreadableIdentity(want,
			fmt.Sprintf("%d of its keys can be read as it, and exactly one may be", matches))
	}

	return found, nil
}

// identityKey reads the next object key as a string, reporting false for
// anything else - a shape a well-formed JSON object cannot produce, and
// therefore the one a malformed document ends this walk on.
func identityKey(dec *json.Decoder) (string, bool) {
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	key, ok := tok.(string)

	return key, ok
}

// readIdentityField reads one string field of the identity object under
// readIdentityObject's rule, since encoding/json folds inner keys too; a
// non-string value is refused rather than coerced.
func readIdentityField(info json.RawMessage, want string) (string, error) {
	raw, err := readIdentityObject(info, want)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", unreadableIdentity(want, "it is not a string")
	}

	return value, nil
}

// unreadableIdentity is the refusal every identity shape renders: it names the
// key and the fault, never the archive-chosen document's own bytes.
func unreadableIdentity(key, why string) error {
	return fmt.Errorf("%w: %s cannot be read for one %q: %s",
		helpers.ErrSignatureAttributionMismatch, helpers.ManifestFileName, key, why)
}

// strictSpelling renders the strict count spelling that closes a vacuous pass;
// a count of zero maps to "+1", because "+0" fails under every outcome of
// signature.Policy's verdict.
func strictSpelling(spec signature.CountSpec) string {
	if spec.All {
		return "+all"
	}
	if spec.Count < 1 {
		return "+1"
	}

	return "+" + strconv.Itoa(spec.Count)
}

// nextBlob pulls col's blobs one at a time, declared sources in file order and
// then server-carried ones, so no gather holds every blob resident. The cap
// counts every candidate consumed; a sha256 of each blob's bytes dedupes repeats.
func (vc *verifyContext) nextBlob(
	ctx context.Context,
	runtime *infra.Infra,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
) signature.NextBlob {
	sources := vc.sources[requirementKey(col)]
	server, offered := serverSignatureBlobs(meta)
	limit := gatherLimit(runtime, col, len(sources), offered)

	var (
		next int
		seen [][sha256.Size]byte
	)

	return func() (signature.Blob, bool, error) {
		for next < limit {
			blob, err := vc.gatherOne(ctx, sources, server, next)
			next++
			if err != nil {
				return signature.Blob{}, false, err
			}
			sum := sha256.Sum256(blob.Data)
			if slices.Contains(seen, sum) {
				continue
			}
			seen = append(seen, sum)

			return blob, true, nil
		}

		return signature.Blob{}, false, nil
	}
}

// gatherLimit caps declared plus server-offered candidates at
// helpers.MaxSignaturesPerCollection, declared first, and warns on truncation.
// With the server slice capped too, limit never exceeds len(sources)+len(server).
func gatherLimit(runtime *infra.Infra, col collection, sources, offered int) int {
	limit := min(sources+offered, helpers.MaxSignaturesPerCollection)
	if dropped := sources + offered - limit; dropped > 0 {
		runtime.Output.Warnf(
			"%s: %d signature candidates exceed the limit of %d (%d declared, %d offered by the server); "+
				"the last %d were not gathered, and declared sources always come first",
			col.key(), sources+offered, helpers.MaxSignaturesPerCollection, sources, offered, dropped)
	}

	return limit
}

// gatherOne produces the i-th blob of the gather: a declared source is fetched
// through FetchRequirementSource, whose client carries no Galaxy token and no
// relaxed TLS; a server-carried blob is already in hand.
func (vc *verifyContext) gatherOne(
	ctx context.Context,
	sources []string,
	server []signature.Blob,
	i int,
) (signature.Blob, error) {
	if i < len(sources) {
		return vc.fetcher.FetchRequirementSource(ctx, sources[i])
	}

	return server[i-len(sources)], nil
}

// serverSignatureBlobs reads the signatures in a server's version metadata,
// skipping malformed, empty or oversized entries rather than failing. It copies
// at most helpers.MaxSignaturesPerCollection but returns the uncapped count.
func serverSignatureBlobs(meta *types.GalaxyCollectionVersionInfo) ([]signature.Blob, int) {
	if meta == nil {
		return nil, 0
	}
	list, ok := meta.Signatures.([]any)
	if !ok || len(list) == 0 {
		return nil, 0
	}

	origin := serverBlobOrigin(meta)
	offered := 0
	blobs := make([]signature.Blob, 0, min(len(list), helpers.MaxSignaturesPerCollection))
	for _, entry := range list {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		value, ok := row[serverSignatureField].(string)
		if !ok || value == "" || int64(len(value)) > helpers.SignatureMaxSize {
			continue
		}
		offered++
		if len(blobs) < helpers.MaxSignaturesPerCollection {
			blobs = append(blobs, signature.Blob{Origin: origin, Data: []byte(value)})
		}
	}

	return blobs, offered
}

// serverBlobOrigin names a server-carried signature's source: the metadata href
// with credentials cut before it is truncated, since truncating first can leave
// part of one behind, or a fixed label when the server sent no href.
func serverBlobOrigin(meta *types.GalaxyCollectionVersionInfo) string {
	if meta.Href != "" {
		return helpers.TruncateForMessage(helpers.WithoutCredentials(meta.Href))
	}

	return "galaxy server version metadata"
}

// signatureDeadlineError relabels err as helpers.ErrSignatureFetchDeadline only
// when sigCtx's own budget ended the work, idempotently. The cause renders with
// %v so context.Canceled cannot make a hung signature host exit as a Ctrl-C.
func signatureDeadlineError(parent, sigCtx context.Context, budget time.Duration, err error) error {
	if err == nil || errors.Is(err, helpers.ErrSignatureFetchDeadline) {
		return err
	}
	if parent.Err() != nil || !errors.Is(sigCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	//nolint:errorlint // deliberately %v, not %w: see the doc comment above and helpers.ErrSignatureFetchDeadline's own.
	return fmt.Errorf("%w after %s: %v", helpers.ErrSignatureFetchDeadline, budget, err)
}

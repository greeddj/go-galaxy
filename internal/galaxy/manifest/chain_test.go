package manifest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The vocabulary of these documents, hand-spelled rather than taken from the
// production constants, so a change to a name, the checksum type or a cap
// fails these tests instead of moving with them.
const (
	testManifestName     = "MANIFEST.json"
	testFilesName        = "FILES.json"
	testFtypeFile        = "file"
	testFtypeDir         = "dir"
	testChksumSHA256     = "sha256"
	testEntryNameCap     = 1024
	testFilesManifestCap = 32 << 20
)

// testOtherDigest is a well-formed sha256 that is not the digest of anything a
// fixture below carries, so a row naming it is wrong about content rather than
// malformed.
const testOtherDigest = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

// chainEntry is one tar entry of a chain fixture together with what FILES.json
// is to say about it. The zero value describes a regular file, listed with its
// own true digest - so a test states only what it changes.
type chainEntry struct {
	// name is the entry's archive path.
	name string
	// linkname is a link entry's target, written verbatim: a symlink's is read
	// relative to the entry's own directory, a hardlink's to the archive root.
	linkname string
	// listAs overrides the ftype the listing row carries. Empty means "dir" for
	// a directory entry and "file" for everything else.
	listAs string
	// listDigest overrides the digest the listing row names.
	listDigest string
	// listChksumType overrides the checksum type the listing row names.
	listChksumType string
	// content is the body FILES.json's digest is computed from.
	content []byte
	// archivedContent is what the tar stream carries instead of content, for a
	// fixture whose listing and archive have to disagree.
	archivedContent []byte
	// declaredSize overrides the size the entry's header announces.
	declaredSize int64
	// typeflag defaults to tar.TypeReg, which is not its own zero value.
	typeflag byte
	// unlisted keeps the entry out of FILES.json.
	unlisted bool
	// unarchived keeps the entry out of the tar stream, leaving its listing row
	// in place.
	unarchived bool
}

// chainSpec describes one fixture. Left unset it yields a well-formed artifact,
// which TestVerifyChainAcceptsWellFormedArtifact accepts, so every refusal is
// that default with exactly one field changed.
type chainSpec struct {
	// mutateFiles rewrites FILES.json before the pointer is computed from it,
	// so a corrupted listing is judged by the listing's own rules.
	mutateFiles func([]byte) []byte
	// mutateManifest rewrites MANIFEST.json after the pointer is written into
	// it; the archive carries the same bytes, so only the pointer is at fault.
	mutateManifest func([]byte) []byte
	// filesLinkname is the FILES.json entry's link target, for the typeflag
	// below.
	filesLinkname string
	// pointerDigest overrides the digest MANIFEST.json names for FILES.json:
	// the one knob that yields a pointer which parses and is wrong.
	pointerDigest string
	entries       []chainEntry
	// filesDeclaredSize overrides the size the FILES.json header announces.
	filesDeclaredSize int64
	// filesTypeflag overrides the typeflag of the FILES.json entry itself.
	filesTypeflag byte
	// omitFilesManifest leaves FILES.json out of the tar stream entirely.
	omitFilesManifest bool
	// omitRootRow drops the "." row a listing otherwise always carries, for
	// the one fixture that needs "." to be an unoccupied key.
	omitRootRow bool
}

// listingRow is one rendered row of FILES.json. The checksum fields are
// pointers so a non-file row renders them as JSON null, as a directory row
// does in these documents.
type listingRow struct {
	ChksumType   *string `json:"chksum_type"`
	ChksumSha256 *string `json:"chksum_sha256"`
	Name         string  `json:"name"`
	Ftype        string  `json:"ftype"`
}

// chainEntries is the content a well-formed fixture carries: a regular file, a
// directory, and a file nested under one.
func chainEntries() []chainEntry {
	return []chainEntry{
		{name: "README.md", content: []byte("# acme.widgets\n")},
		{name: "plugins", typeflag: tar.TypeDir},
		{name: "plugins/modules/widget.py", content: []byte("# widget\n")},
	}
}

// buildChainArtifact writes one fixture to disk and returns its path with the
// MANIFEST.json bytes a signature would have been verified over, the same
// bytes the archive's own MANIFEST.json entry carries.
func buildChainArtifact(tb testing.TB, spec chainSpec) (string, []byte) {
	tb.Helper()

	filesJSON := renderFilesListing(tb, spec)
	if spec.mutateFiles != nil {
		filesJSON = spec.mutateFiles(filesJSON)
	}
	pointer := hexDigest(filesJSON)
	if spec.pointerDigest != "" {
		pointer = spec.pointerDigest
	}
	manifestJSON := renderChainManifest(tb, pointer)
	if spec.mutateManifest != nil {
		manifestJSON = spec.mutateManifest(manifestJSON)
	}

	stream := []testEntry{{name: testManifestName, content: manifestJSON}}
	if !spec.omitFilesManifest {
		stream = append(stream, filesManifestEntry(spec, filesJSON))
	}
	for _, e := range spec.entries {
		if e.unarchived {
			continue
		}
		body := e.content
		if e.archivedContent != nil {
			body = e.archivedContent
		}
		stream = append(stream, testEntry{
			name: e.name, linkname: e.linkname, content: body,
			declaredSize: e.declaredSize, typeflag: e.typeflag,
		})
	}
	return writeArtifact(tb, stream), manifestJSON
}

// filesManifestEntry renders the archive's FILES.json entry, which carries the
// listing's bytes unless the fixture asked for some other typeflag - a link
// entry cannot carry a body at all.
func filesManifestEntry(spec chainSpec, filesJSON []byte) testEntry {
	if spec.filesTypeflag != 0 {
		return testEntry{name: testFilesName, linkname: spec.filesLinkname, typeflag: spec.filesTypeflag}
	}
	return testEntry{name: testFilesName, content: filesJSON, declaredSize: spec.filesDeclaredSize}
}

// renderFilesListing builds the listing a well-formed collection would carry:
// the "." row naming the collection root, then one row per listed entry.
func renderFilesListing(tb testing.TB, spec chainSpec) []byte {
	tb.Helper()

	entries := spec.entries
	digests := chainDigests(entries)
	rows := make([]listingRow, 0, len(entries)+1)
	if !spec.omitRootRow {
		rows = append(rows, listingRow{Name: ".", Ftype: testFtypeDir})
	}
	for _, e := range entries {
		if e.unlisted {
			continue
		}
		rows = append(rows, e.listingRow(digests))
	}

	body, err := json.Marshal(struct {
		Files []listingRow `json:"files"`
	}{Files: rows})
	if err != nil {
		tb.Fatalf("failed to render the fixture's %s: %v", testFilesName, err)
	}
	return body
}

// listingRow renders one entry's row, taking its digest from what the archive
// really carries unless the fixture overrode it.
func (e chainEntry) listingRow(digests map[string]string) listingRow {
	row := listingRow{Name: e.name, Ftype: e.listingFtype()}
	if row.Ftype != testFtypeFile {
		return row
	}
	chksumType := e.listChksumType
	if chksumType == "" {
		chksumType = testChksumSHA256
	}
	digest := e.listDigest
	if digest == "" {
		digest = digests[path.Clean(e.name)]
	}
	row.ChksumType = &chksumType
	row.ChksumSha256 = &digest
	return row
}

func (e chainEntry) listingFtype() string {
	if e.listAs != "" {
		return e.listAs
	}
	if e.typeflagOrDefault() == tar.TypeDir {
		return testFtypeDir
	}
	return testFtypeFile
}

func (e chainEntry) typeflagOrDefault() byte {
	if e.typeflag == 0 {
		return tar.TypeReg
	}
	return e.typeflag
}

// chainDigests returns the digest a truthful FILES.json names for each entry,
// resolving links independently of the reader under test; an entry whose
// target the fixture does not carry gets none, which makes a dangling link.
func chainDigests(entries []chainEntry) map[string]string {
	contents := make(map[string][]byte)
	links := make(map[string]string)
	for _, e := range entries {
		key := path.Clean(e.name)
		switch e.typeflagOrDefault() {
		case tar.TypeReg:
			contents[key] = e.content
		case tar.TypeSymlink:
			links[key] = path.Join(path.Dir(key), e.linkname)
		case tar.TypeLink:
			links[key] = path.Clean(e.linkname)
		}
	}

	out := make(map[string]string, len(contents)+len(links))
	for key, body := range contents {
		out[key] = hexDigest(body)
	}
	for key := range links {
		if body, ok := followFixtureLink(contents, links, key); ok {
			out[key] = hexDigest(body)
		}
	}
	return out
}

// followFixtureLink walks the fixture's own link map to the content it names,
// stopping well past any chain a test builds so a cycle cannot hang the suite.
func followFixtureLink(contents map[string][]byte, links map[string]string, key string) ([]byte, bool) {
	for range len(links) + 1 {
		if body, ok := contents[key]; ok {
			return body, true
		}
		next, ok := links[key]
		if !ok {
			return nil, false
		}
		key = next
	}
	return nil, false
}

// renderChainManifest builds the signed document, pointing at a listing with
// the given digest.
func renderChainManifest(tb testing.TB, filesDigest string) []byte {
	tb.Helper()

	body, err := json.Marshal(map[string]any{
		"collection_info": map[string]string{"namespace": "acme", "name": "widgets", "version": "1.0.0"},
		"file_manifest_file": map[string]string{
			"name":          testFilesName,
			"ftype":         testFtypeFile,
			"chksum_type":   testChksumSHA256,
			"chksum_sha256": filesDigest,
		},
	})
	if err != nil {
		tb.Fatalf("failed to render the fixture's %s: %v", testManifestName, err)
	}
	return body
}

func hexDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// replaceOnce returns a document hook rewriting the first occurrence of
// needle, failing the test when it is absent so a fixture cannot quietly stop
// corrupting what it was written to corrupt.
func replaceOnce(t *testing.T, needle, replacement string) func([]byte) []byte {
	t.Helper()

	return func(body []byte) []byte {
		if !bytes.Contains(body, []byte(needle)) {
			t.Fatalf("the rendered document does not contain %q", needle)
		}
		return bytes.Replace(body, []byte(needle), []byte(replacement), 1)
	}
}

// verifyChainFixture builds a fixture and runs the check over it.
func verifyChainFixture(t *testing.T, spec chainSpec) error {
	t.Helper()

	artifact, manifestJSON := buildChainArtifact(t, spec)
	return VerifyChain(t.Context(), artifact, manifestJSON)
}

func TestVerifyChainAcceptsWellFormedArtifact(t *testing.T) {
	t.Parallel()

	// The positive control every refusal rests on: the builder's default output,
	// with its "." row and that row's null checksums, is accepted.
	if err := verifyChainFixture(t, chainSpec{entries: chainEntries()}); err != nil {
		t.Fatalf("VerifyChain(well-formed artifact) error = %v, want no error", err)
	}
}

func TestVerifyChainRejectsMismatchedFilesManifestDigest(t *testing.T) {
	t.Parallel()

	// The pointer in the signed document against the listing the archive
	// carries. It names a well-formed sha256 that is not this listing's, since a
	// malformed one never reaches the comparison.
	spec := chainSpec{entries: chainEntries(), pointerDigest: testOtherDigest}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(pointer naming another digest) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsModifiedListedFile(t *testing.T) {
	t.Parallel()

	// The third link: a listed file's own bytes. The listing names the digest
	// of content, the archive carries archivedContent, and nothing else in the
	// fixture moves.
	entries := chainEntries()
	entries[0].archivedContent = []byte("# tampered\n")

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listed file with other bytes) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsMissingListedFile(t *testing.T) {
	t.Parallel()

	entries := chainEntries()
	entries[2].unarchived = true

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listed file absent from the archive) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsUnlistedArchiveEntry(t *testing.T) {
	t.Parallel()

	// The injection the reverse rule exists for: every listed file matches, yet
	// the archive carries a file nobody vouched for, which the extractor would
	// install because it unpacks the whole archive.
	entries := append(chainEntries(), chainEntry{
		name: "plugins/modules/backdoor.py", content: []byte("# injected\n"), unlisted: true,
	})

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(archive carrying an unlisted file) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsEmptyListingWithContent(t *testing.T) {
	t.Parallel()

	// An empty listing gives the forward walk nothing to compare, so only the
	// reverse rule refuses content that MANIFEST.json correctly vouches for.
	spec := chainSpec{
		entries: chainEntries(),
		mutateFiles: func([]byte) []byte {
			return []byte(`{"files":[]}`)
		},
	}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(empty listing over a non-empty archive) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsNonGzip(t *testing.T) {
	t.Parallel()

	// The manifest is well-formed and its pointer parses, so the refusal is the
	// gzip header answering rather than anything read out of the document. The
	// bytes are what a proxy or a login page delivers in an artifact's place.
	_, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})

	page := writeFile(t, "error-page.html", []byte("<html><body>404 Not Found</body></html>"))
	if err := VerifyChain(t.Context(), page, manifestJSON); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("VerifyChain(an error page) error = %v, want the shape sentinel", err)
	}
}

func TestVerifyChainRejectsUnparseableListing(t *testing.T) {
	t.Parallel()

	// MANIFEST.json vouches for a listing that does not parse, which must be a
	// refusal rather than an empty listing; the archive carries only the two
	// exempt documents so the reverse rule cannot answer in its place.
	spec := chainSpec{
		mutateFiles: func([]byte) []byte {
			return []byte(`{"files":[{"name":`)
		},
	}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listing that does not parse) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainExemptsAListedFilesManifestButNotAListedManifest(t *testing.T) {
	t.Parallel()

	// A FILES.json row is skipped, so its wrong digest is accepted, while a
	// MANIFEST.json row is checked and can only fail. Both carry the same wrong
	// digest, so only the rule separates them.
	cases := []struct {
		name    string
		listed  string
		wantErr bool
	}{
		{name: "a row for FILES.json", listed: testFilesName},
		{name: "a row for MANIFEST.json", listed: testManifestName, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			row := listingFileRowJSON(tc.listed, testOtherDigest)
			spec := chainSpec{
				entries:     chainEntries(),
				mutateFiles: replaceOnce(t, `{"files":[`, `{"files":[`+row+`,`),
			}

			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(listing carrying %s) error = %v, want the chain sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(listing carrying %s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// listingFileRowJSON renders one file row exactly as json.Marshal renders
// listingRow, so a test can splice a row the builder would never produce into
// an otherwise well-formed listing.
func listingFileRowJSON(name, digest string) string {
	return `{"chksum_type":"` + testChksumSHA256 + `","chksum_sha256":"` + digest +
		`","name":"` + name + `","ftype":"` + testFtypeFile + `"}`
}

func TestVerifyChainRejectsUnsafeListedName(t *testing.T) {
	t.Parallel()

	// A listed name is judged like a tar entry's own, as the listing's fault.
	// Every row is a directory row, which is never resolved against the archive,
	// so only this rule can refuse it.
	cases := []struct {
		name        string
		listed      string
		omitRootRow bool
	}{
		{name: "absolute", listed: "/etc/passwd"},
		{name: "escaping", listed: "../../etc/passwd"},
		// An empty name cleans to ".", the root row's key, so the root row is
		// dropped to keep the duplicate-row rule from answering instead.
		{name: "empty", listed: "", omitRootRow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := append(chainEntries(), chainEntry{
				name: tc.listed, listAs: testFtypeDir, unarchived: true,
			})

			err := verifyChainFixture(t, chainSpec{entries: entries, omitRootRow: tc.omitRootRow})
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(listing naming %q) error = %v, want the chain sentinel", tc.listed, err)
			}
		})
	}
}

func TestVerifyChainRejectsUnsafeArchiveEntryPath(t *testing.T) {
	t.Parallel()

	// The same three shapes on the archive side. Each entry is unlisted and each
	// row asserts its own sentinel, since the reverse rule would otherwise refuse
	// it under a different one.
	cases := []struct {
		want error
		name string
		path string
	}{
		{helpers.ErrArchiveEntryIsAbsolutePath, "absolute", "/etc/passwd"},
		{helpers.ErrArchiveEntryEscapesDestination, "escaping", "../evil.txt"},
		{helpers.ErrArchiveEntryHasEmptyName, "empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := append(chainEntries(), chainEntry{name: tc.path, unlisted: true})

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if !errors.Is(err, tc.want) {
				t.Fatalf("VerifyChain(archive entry named %q) error = %v, want %v", tc.path, err, tc.want)
			}
		})
	}
}

func TestChargeEntrySizeRefusesOutOfBudgetEntries(t *testing.T) {
	t.Parallel()

	// Driven straight at chargeEntrySize because a negative size and the 4 GiB
	// archive total are unreachable from a fixture;
	// TestVerifyChainChargesEveryEntryHeader covers the call site.
	cases := []struct {
		want     error
		name     string
		size     int64
		declared int64
		wantSum  int64
	}{
		{name: "an ordinary entry is added to the total", size: 100, declared: 40, wantSum: 140},
		{name: "an entry at the per-entry cap", size: helpers.ArchiveMaxEntrySize, wantSum: helpers.ArchiveMaxEntrySize},
		{
			name: "an entry filling the per-archive total exactly", size: helpers.ArchiveMaxEntrySize,
			declared: helpers.ArchiveMaxTotalSize - helpers.ArchiveMaxEntrySize, wantSum: helpers.ArchiveMaxTotalSize,
		},
		{name: "a negative size", size: -1, want: helpers.ErrArchiveEntryHasNegativeSize},
		{
			name: "one byte past the per-entry cap", size: helpers.ArchiveMaxEntrySize + 1,
			want: helpers.ErrArchiveEntryIsTooLarge,
		},
		{
			name: "one byte past the per-archive total", size: helpers.ArchiveMaxEntrySize,
			declared: helpers.ArchiveMaxTotalSize - helpers.ArchiveMaxEntrySize + 1,
			want:     helpers.ErrArchiveExceedsMaxSize,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			declared := tc.declared
			err := chargeEntrySize(&tar.Header{Typeflag: tar.TypeReg, Name: "e.bin", Size: tc.size}, &declared)
			checkChargeOutcome(t, tc.want, err, declared, tc.declared, tc.wantSum)
		})
	}
}

// checkChargeOutcome asserts one chargeEntrySize outcome: the sentinel, and a
// running total left unchanged by a refusal and advanced by a success.
func checkChargeOutcome(t *testing.T, want, got error, declared, before, wantSum int64) {
	t.Helper()

	if want == nil {
		if got != nil {
			t.Fatalf("chargeEntrySize onto %d error = %v, want no error", before, got)
		}
		if declared != wantSum {
			t.Fatalf("chargeEntrySize onto %d left the total at %d, want %d", before, declared, wantSum)
		}
		return
	}
	if !errors.Is(got, want) {
		t.Fatalf("chargeEntrySize onto %d error = %v, want %v", before, got, want)
	}
	if declared != before {
		t.Fatalf("chargeEntrySize onto %d refused and still moved the total to %d", before, declared)
	}
}

func TestVerifyChainChargesEveryEntryHeader(t *testing.T) {
	t.Parallel()

	// The call site the table above cannot prove exists: an entry declaring a
	// byte past the per-entry cap with no body is refused off its header.
	entries := append(chainEntries(), chainEntry{
		name: "huge.bin", declaredSize: helpers.ArchiveMaxEntrySize + 1, unlisted: true,
	})

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("VerifyChain(entry declaring past the per-entry cap) error = %v, want the entry-size sentinel", err)
	}
}

func TestVerifyChainAcceptsSymlinkHashedAsItsTarget(t *testing.T) {
	t.Parallel()

	// Both symlink shapes, beside the target and reaching up out of its own
	// directory, listed with the target's digest, which is what the reference
	// reader and this tool's extractor both resolve a link to.
	entries := []chainEntry{
		{name: "docs/real.md", content: []byte("real\n")},
		{name: "docs/alias.md", linkname: "real.md", typeflag: tar.TypeSymlink},
		{name: "meta/alias.md", linkname: "../docs/real.md", typeflag: tar.TypeSymlink},
	}

	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(symlinks hashed as their targets) error = %v, want no error", err)
	}
}

func TestVerifyChainRejectsSymlinkWithMismatchedTargetDigest(t *testing.T) {
	t.Parallel()

	// The negative of the test above, on the identical fixture with one field
	// changed: the link is listed against a digest that is not its target's.
	entries := []chainEntry{
		{name: "docs/real.md", content: []byte("real\n")},
		{name: "docs/alias.md", linkname: "real.md", typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
	}

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(symlink listed against another digest) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRefusesUnsafeSymlinkTargets(t *testing.T) {
	t.Parallel()

	// Each row asserts its own sentinel: an unsafe target that slipped past its
	// rule would still be refused later under the chain sentinel.
	cases := []struct {
		want     error
		name     string
		linkname string
	}{
		{nil, "in-archive target", "real.md"},
		{helpers.ErrSymlinkTargetIsEmpty, "empty target", ""},
		{helpers.ErrSymlinkTargetEscapesDestination, "escaping target", "../../../etc/passwd"},
		// One directory up from an entry in docs/ resolves to the archive root,
		// which names no file - a shape the escape test only catches because it
		// checks for "." as well as for a leading "..".
		{helpers.ErrSymlinkTargetEscapesDestination, "target resolving to the root", ".."},
		// Pins the check order: tested after path.Join, "/etc/passwd" would become
		// "docs/etc/passwd" and be recorded as an ordinary link.
		{helpers.ErrSymlinkTargetIsAbsolute, "absolute target", "/etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := []chainEntry{
				{name: "docs/real.md", content: []byte("real\n")},
				{name: "docs/alias.md", linkname: tc.linkname, typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
			}
			// The accepting row names its target truthfully, so the digest
			// override above has to be dropped for it.
			if tc.want == nil {
				entries[1].listDigest = ""
			}

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if tc.want == nil && err != nil {
				t.Fatalf("VerifyChain(symlink to %q) error = %v, want no error", tc.linkname, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("VerifyChain(symlink to %q) error = %v, want %v", tc.linkname, err, tc.want)
			}
		})
	}
}

func TestVerifyChainAcceptsHardlinkHashedAsItsTarget(t *testing.T) {
	t.Parallel()

	// A hardlink's target is read against the archive root, not the entry's
	// directory: the first two rows pin that base from opposite sides, and the
	// last two name no file at all.
	cases := []struct {
		want     error
		name     string
		linkname string
	}{
		{nil, "root-relative target", "docs/real.md"},
		{helpers.ErrArchiveEntryEscapesDestination, "directory-relative target", "../docs/real.md"},
		{helpers.ErrHardlinkTargetIsEmpty, "empty target", ""},
		{helpers.ErrHardlinkTargetIsEmpty, "target naming the archive root", "."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := []chainEntry{
				{name: "docs/real.md", content: []byte("real\n")},
				{name: "meta/hard.md", linkname: tc.linkname, typeflag: tar.TypeLink},
			}
			if tc.want != nil {
				entries[1].listDigest = testOtherDigest
			}

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if tc.want == nil && err != nil {
				t.Fatalf("VerifyChain(hardlink to %q) error = %v, want no error", tc.linkname, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("VerifyChain(hardlink to %q) error = %v, want %v", tc.linkname, err, tc.want)
			}
		})
	}
}

func TestVerifyChainAcceptsUnlistedDirectories(t *testing.T) {
	t.Parallel()

	// A directory carries no content to digest, so both a listed one with null
	// checksums and an unlisted one are accepted.
	entries := []chainEntry{
		{name: "roles", typeflag: tar.TypeDir},
		{name: "plugins", typeflag: tar.TypeDir, unlisted: true},
		{name: "plugins/widget.py", content: []byte("# widget\n")},
	}

	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(listed and unlisted directories) error = %v, want no error", err)
	}
}

func TestVerifyChainAcceptsSymlinkToDirectoryListedAsDir(t *testing.T) {
	t.Parallel()

	// A symlink to a directory is listed with ftype dir, so "listed" must mean
	// "appears in files at all" or this legitimate collection is refused.
	entries := []chainEntry{
		{name: "plugins/modules", typeflag: tar.TypeDir},
		{name: "plugins/alias", linkname: "modules", typeflag: tar.TypeSymlink, listAs: testFtypeDir},
	}

	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(symlink to a directory listed as dir) error = %v, want no error", err)
	}
}

func TestVerifyChainRejectsMalformedListedDigest(t *testing.T) {
	t.Parallel()

	// Every shape a digest can take and not be one. The JSON-type rows pin that
	// null decodes to the empty string and a number or bool to a type error, and
	// both are refused.
	trueRow := `"chksum_sha256":"` + hexDigest([]byte("# acme.widgets\n")) + `"`
	cases := []struct {
		name        string
		digest      string
		needle      string
		replacement string
	}{
		{name: "64 characters that are not hex", digest: strings.Repeat("z", 64)},
		{name: "one character short", digest: testOtherDigest[:63]},
		{name: "one character long", digest: testOtherDigest + "a"},
		{name: "uppercase", digest: strings.ToUpper(testOtherDigest)},
		// An empty listDigest means "use the true digest" to the builder, so
		// the row asks for a real one and then rewrites it away.
		{name: "empty", digest: testOtherDigest, needle: testOtherDigest, replacement: ""},
		{name: "json number", needle: trueRow, replacement: `"chksum_sha256":42`},
		{name: "json bool", needle: trueRow, replacement: `"chksum_sha256":true`},
		{name: "json null", needle: trueRow, replacement: `"chksum_sha256":null`},
		{name: "absent", needle: trueRow + ",", replacement: ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := chainEntries()
			entries[0].listDigest = tc.digest
			spec := chainSpec{entries: entries}
			if tc.needle != "" {
				spec.mutateFiles = replaceOnce(t, tc.needle, tc.replacement)
			}

			err := verifyChainFixture(t, spec)
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(listed digest %s) error = %v, want the chain sentinel", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsUnsupportedListedChecksumType(t *testing.T) {
	t.Parallel()

	// The listed digest is left truthful, so what is refused is the algorithm
	// the row names rather than the value under it.
	entries := chainEntries()
	entries[0].listChksumType = "sha512"

	err := verifyChainFixture(t, chainSpec{entries: entries})
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listing naming sha512) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsMalformedChainPointer(t *testing.T) {
	t.Parallel()

	// The pointer is read from the signed document before the archive. A type
	// error still decodes the rest of the pointer, so only the decode error check
	// refuses the "wrong type inside the pointer" row.
	cases := []struct {
		name          string
		document      string
		needle        string
		replacement   string
		pointerDigest string
	}{
		{name: "syntax error in the manifest", document: "{not json"},
		{
			name:   "wrong type inside the pointer",
			needle: `"chksum_type":"` + testChksumSHA256 + `"`, replacement: `"chksum_type":42`,
		},
		{name: "pointer absent", document: `{"collection_info":{"name":"widgets"}}`},
		{name: "pointer names another file", needle: `"name":"` + testFilesName + `"`, replacement: `"name":"OTHER.json"`},
		{name: "digest of 64 characters that are not hex", pointerDigest: strings.Repeat("z", 64)},
		{name: "digest one character short", pointerDigest: testOtherDigest[:63]},
		{name: "digest one character long", pointerDigest: testOtherDigest + "a"},
		{name: "digest uppercase", pointerDigest: strings.ToUpper(testOtherDigest)},
		// An empty pointerDigest means "use the true digest" to the builder,
		// so the row asks for a real one and then rewrites it away.
		{name: "digest empty", pointerDigest: testOtherDigest, needle: testOtherDigest, replacement: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: chainEntries(), pointerDigest: tc.pointerDigest}
			switch {
			case tc.document != "":
				spec.mutateManifest = func([]byte) []byte { return []byte(tc.document) }
			case tc.needle != "":
				spec.mutateManifest = replaceOnce(t, tc.needle, tc.replacement)
			}

			err := verifyChainFixture(t, spec)
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want the chain sentinel", tc.name, err)
			}
		})
	}
}

func TestVerifyChainReadsThePointerBeforeTheArchive(t *testing.T) {
	t.Parallel()

	// A malformed pointer is refused before a byte of the artifact is read, so a
	// non-archive yields the chain sentinel rather than the shape one; over a real
	// archive this rule and the digest comparison share one sentinel.
	_, manifestJSON := buildChainArtifact(t, chainSpec{
		entries:       chainEntries(),
		pointerDigest: strings.Repeat("z", 64),
	})

	notAnArchive := writeFile(t, "not-an-archive.bin", []byte("<html>404</html>"))
	err := VerifyChain(t.Context(), notAnArchive, manifestJSON)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(malformed pointer over a non-archive) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsMissingFilesManifest(t *testing.T) {
	t.Parallel()

	// Both ways the listing can be missing as a regular file. The symlink row
	// matters on its own: a reader asking only "is there an entry called
	// FILES.json" would find one, and would then have nothing to decode.
	cases := []struct {
		name string
		spec chainSpec
	}{
		{
			name: "absent entirely",
			spec: chainSpec{entries: chainEntries(), omitFilesManifest: true},
		},
		{
			name: "present as a symlink",
			spec: chainSpec{
				entries:       chainEntries(),
				filesTypeflag: tar.TypeSymlink,
				filesLinkname: "elsewhere/list.json",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, tc.spec)
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s %s) error = %v, want the chain sentinel", testFilesName, tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsForeignManifestBytes(t *testing.T) {
	t.Parallel()

	// The seam: the signed bytes must be the archive's own MANIFEST.json. The
	// foreign document names this listing's true digest, so only the seam can
	// refuse a signed manifest stapled onto another tarball.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})

	foreign := bytes.Replace(manifestJSON, []byte(`"version":"1.0.0"`), []byte(`"version":"9.9.9"`), 1)
	if bytes.Equal(foreign, manifestJSON) {
		t.Fatalf("the fixture's %s no longer carries the version this test rewrites", testManifestName)
	}

	err := VerifyChain(t.Context(), artifact, foreign)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(another artifact's manifest) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsDuplicateArchiveEntry(t *testing.T) {
	t.Parallel()

	// Duplicate names are refused, the two documents included: ReadFromTarGz
	// takes the first such entry and an unrefused chain walk the last, so an
	// exemption would let the two passes see different documents.
	cases := []struct {
		name      string
		duplicate chainEntry
	}{
		{name: "an ordinary file", duplicate: chainEntry{name: "README.md", content: []byte("# second\n")}},
		{name: testManifestName, duplicate: chainEntry{name: testManifestName, content: []byte(`{"decoy":true}`)}},
		{name: testFilesName, duplicate: chainEntry{name: testFilesName, content: []byte(`{"files":[]}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The duplicate is an entry no listing vouches for, which is the
			// shape this refusal exists to answer, and keeping it out leaves
			// the fixture's listing the well-formed one it would be without it.
			duplicate := tc.duplicate
			duplicate.unlisted = true

			err := verifyChainFixture(t, chainSpec{entries: append(chainEntries(), duplicate)})
			if !errors.Is(err, helpers.ErrArchiveDuplicateEntry) {
				t.Fatalf("VerifyChain(a second %s) error = %v, want the duplicate sentinel", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsDuplicateListingEntry(t *testing.T) {
	t.Parallel()

	// A listing naming one path twice contradicts itself: one of the two rows
	// decides, and which one it is would be an artifact of iteration order.
	trueDigest := hexDigest([]byte("# acme.widgets\n"))
	row := `{"chksum_type":"` + testChksumSHA256 + `","chksum_sha256":"` + trueDigest +
		`","name":"README.md","ftype":"` + testFtypeFile + `"}`
	spec := chainSpec{
		entries:     chainEntries(),
		mutateFiles: replaceOnce(t, row, row+","+row),
	}

	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("VerifyChain(listing naming one path twice) error = %v, want the chain sentinel", err)
	}
}

func TestVerifyChainRejectsOverlongEntryName(t *testing.T) {
	t.Parallel()

	// This pass holds every entry name until the stream ends, so it caps names and
	// link targets. The lengths are literals so a change to
	// helpers.ArchiveMaxEntryNameLen fails a row instead of moving with it.
	cases := []struct {
		name    string
		nameLen int
		linkLen int
		wantErr bool
	}{
		{name: "name at the cap", nameLen: testEntryNameCap},
		{name: "name one byte over", nameLen: testEntryNameCap + 1, wantErr: true},
		{name: "link target at the cap", nameLen: 16, linkLen: testEntryNameCap},
		{name: "link target one byte over", nameLen: 16, linkLen: testEntryNameCap + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: longNameEntries(tc.nameLen, tc.linkLen)})
			if tc.wantErr && !errors.Is(err, helpers.ErrArchiveEntryNameTooLong) {
				t.Fatalf("VerifyChain(%s) error = %v, want the name-length sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// longNameEntries builds a fixture whose one entry names itself, or its link
// target, in the given number of bytes. Past the cap the link dangles unlisted,
// since a file under the target's name would breach the same rule first.
func longNameEntries(nameLen, linkLen int) []chainEntry {
	name := strings.Repeat("a", nameLen)
	if linkLen == 0 {
		return []chainEntry{{name: name, content: []byte("body\n")}}
	}
	target := strings.Repeat("b", linkLen)
	link := chainEntry{name: name, linkname: target, typeflag: tar.TypeSymlink}
	if linkLen > testEntryNameCap {
		link.unlisted = true
		return []chainEntry{link}
	}
	return []chainEntry{link, {name: target, content: []byte("body\n")}}
}

func TestVerifyChainRejectsOversizeFilesManifest(t *testing.T) {
	t.Parallel()

	// FILES.json is read into memory whole, so an over-cap declared size is
	// refused off its header before a byte is read. The control has an honest
	// header.
	if err := verifyChainFixture(t, chainSpec{entries: chainEntries()}); err != nil {
		t.Fatalf("VerifyChain(%s with an honest header) error = %v, want no error", testFilesName, err)
	}

	spec := chainSpec{entries: chainEntries(), filesDeclaredSize: testFilesManifestCap + 1}
	err := verifyChainFixture(t, spec)
	if !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("VerifyChain(%s declaring past its cap) error = %v, want the entry-size sentinel", testFilesName, err)
	}
}

func TestVerifyChainRejectsDecompressedStreamOverrun(t *testing.T) {
	t.Parallel()

	// The stream cap through the injected seam, so the fixture stays a kilobyte;
	// the control runs the same bytes under the production cap.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})
	//nolint:gosec // artifact is a path this test just wrote under its own temp directory.
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("failed to read back the fixture: %v", err)
	}

	if err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize); err != nil {
		t.Fatalf("verifyChainStream(under the production cap) error = %v, want no error", err)
	}

	const tinyCap = 1024
	err = verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, tinyCap)
	if !errors.Is(err, helpers.ErrArchiveDecompressedTooLarge) {
		t.Fatalf("verifyChainStream(cap of %d bytes) error = %v, want the stream-cap sentinel", tinyCap, err)
	}
}

func TestVerifyChainRejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	// Pins helpers.ArchiveMaxEntryCount itself over a raw header-only tar stream,
	// with a control at exactly the cap. No test-only cap is injected, which makes
	// this the slowest test in the package.
	if _, err := scanArchive(tar.NewReader(bytes.NewReader(headerOnlyStream(t, helpers.ArchiveMaxEntryCount)))); err != nil {
		t.Fatalf("scanArchive(exactly the entry cap) error = %v, want no error", err)
	}

	_, err := scanArchive(tar.NewReader(bytes.NewReader(headerOnlyStream(t, helpers.ArchiveMaxEntryCount+1))))
	if !errors.Is(err, helpers.ErrArchiveTooManyEntries) {
		t.Fatalf("scanArchive(one entry past the cap) error = %v, want the entry-count sentinel", err)
	}
}

// headerOnlyStream renders count zero-byte regular entries as a raw tar stream.
func headerOnlyStream(t *testing.T, count int64) []byte {
	t.Helper()

	var buf bytes.Buffer
	buf.Grow(int(count+2) * 512)
	tw := tar.NewWriter(&buf)
	for i := range count {
		header := &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("e/%d", i), Mode: 0o644}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("failed to write header %d: %v", i, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close the header-only stream: %v", err)
	}
	return buf.Bytes()
}

func TestVerifyChainStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	// The context is read where the archive is, not before it: the pointer is
	// parsed out of the signed document first, so a fixture whose manifest is
	// well-formed is what proves the cancellation reached the tar walk.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := VerifyChain(ctx, artifact, manifestJSON)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyChain(canceled context) error = %v, want context.Canceled reachable", err)
	}
}

func TestVerifyChainResolvesLinkChainWithinTheHopBound(t *testing.T) {
	t.Parallel()

	// The reference reader resolves a link to a link, so the bound is on chain
	// length: both ends of the permitted range, one hop past it, and a cycle.
	cases := []struct {
		name    string
		entries []chainEntry
		wantErr bool
	}{
		{name: "two hops", entries: linkChainEntries(2)},
		{name: "at the hop bound", entries: linkChainEntries(chainMaxLinkHops)},
		{name: "one hop past the bound", entries: linkChainEntries(chainMaxLinkHops + 1), wantErr: true},
		{name: "cycle", entries: linkCycleEntries(), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: tc.entries})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want the chain sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// linkChainEntries builds a file with links long ahead of it: link0 points at
// link1, and the last one points at the file, so resolving link0 costs exactly
// links hops.
func linkChainEntries(links int) []chainEntry {
	entries := make([]chainEntry, 0, links+1)
	entries = append(entries, chainEntry{name: "real.md", content: []byte("real\n")})
	for i := range links {
		target := "real.md"
		if i+1 < links {
			target = fmt.Sprintf("link%d.md", i+1)
		}
		entries = append(entries, chainEntry{
			name: fmt.Sprintf("link%d.md", i), linkname: target, typeflag: tar.TypeSymlink,
		})
	}
	return entries
}

// linkCycleEntries builds two links pointing at each other. The rows carry a
// digest of their own because the fixture's resolver reaches no content for
// them either, which is the point: the hop bound is what ends the walk.
func linkCycleEntries() []chainEntry {
	return []chainEntry{
		{name: "a.md", linkname: "b.md", typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
		{name: "b.md", linkname: "a.md", typeflag: tar.TypeSymlink, listDigest: testOtherDigest},
	}
}

// chainStreamFixture builds a fixture and returns its compressed bytes with the
// signed manifest, for a test that reruns one archive or needs a stable failure
// line, since VerifyChain prefixes its error with a fresh temp path.
func chainStreamFixture(t *testing.T, spec chainSpec) ([]byte, []byte) {
	t.Helper()

	artifact, manifestJSON := buildChainArtifact(t, spec)
	//nolint:gosec // artifact is a path this test just wrote under its own temp directory.
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("failed to read back the fixture: %v", err)
	}
	return raw, manifestJSON
}

// reportedUnlistedEntry pulls the quoted path out of the reverse rule's
// refusal, so a test can assert WHICH entry was named as one comparison and
// fail with a line naming both paths instead of repeating the whole message.
func reportedUnlistedEntry(t *testing.T, err error) string {
	t.Helper()

	_, rest, found := strings.Cut(err.Error(), "the archive carries ")
	name, _, complete := strings.Cut(rest, ", which")
	if !found || !complete {
		t.Fatalf("error = %v, want the reverse rule's own refusal", err)
	}
	return name
}

func TestVerifyChainNamesTheFirstUnlistedEntryInStreamOrder(t *testing.T) {
	t.Parallel()

	// The reverse rule names the first unlisted entry in stream order, so an
	// operator can find it. Two unlisted entries whose stream and lexical orders
	// disagree separate that from a walk over the scan's maps.
	entries := append(chainEntries(),
		chainEntry{name: "plugins/zebra.py", content: []byte("# first in the stream\n")},
		chainEntry{name: "plugins/alpha.py", content: []byte("# second in the stream\n")},
	)

	// The control: the identical archive with both entries listed is accepted,
	// so what the runs below report is the reverse rule rather than the fixture.
	if err := verifyChainFixture(t, chainSpec{entries: entries}); err != nil {
		t.Fatalf("VerifyChain(both entries listed) error = %v, want no error", err)
	}

	first, second := &entries[len(entries)-2], &entries[len(entries)-1]
	first.unlisted, second.unlisted = true, true
	raw, manifestJSON := chainStreamFixture(t, chainSpec{entries: entries})

	// Rerun many times: a map-ordered walk agrees with stream order most of the
	// time, so one run proves nothing and 128 make a miss vanishingly unlikely.
	const runs = 128
	wanted := fmt.Sprintf("%q", first.name)
	for run := range runs {
		err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("verifyChainStream(two unlisted entries) run %d error = %v, want the chain sentinel", run, err)
		}
		if got := reportedUnlistedEntry(t, err); got != wanted {
			t.Fatalf("verifyChainStream(two unlisted entries) named %s on run %d, want %s", got, run, wanted)
		}
	}
}

func TestVerifyChainSkipsEntriesNamingTheArchiveRoot(t *testing.T) {
	t.Parallel()

	// Entries named "." and "foo/.." normalize to the archive root and are skipped,
	// matching the extractor, rather than refused or held against the reverse rule;
	// no listing row could name them.
	raw, manifestJSON := chainStreamFixture(t, chainSpec{entries: append(chainEntries(),
		chainEntry{name: ".", content: []byte("# root\n"), unlisted: true},
		chainEntry{name: "foo/..", content: []byte("# also root\n"), unlisted: true},
	)})

	err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
	if err != nil {
		t.Fatalf("verifyChainStream(root-named entries) error = %v, want no error", err)
	}
}

func TestVerifyChainRefusesAnArtifactItCannotOpen(t *testing.T) {
	t.Parallel()

	// The open is the one step VerifyChain takes before the walk: an artifact
	// evicted between download and check is refused as missing, while the same
	// fixture at its real path is accepted.
	artifact, manifestJSON := buildChainArtifact(t, chainSpec{entries: chainEntries()})
	if err := VerifyChain(t.Context(), artifact, manifestJSON); err != nil {
		t.Fatalf("VerifyChain(the fixture where it sits) error = %v, want no error", err)
	}

	if err := VerifyChain(t.Context(), artifact+".absent", manifestJSON); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("VerifyChain(a path carrying no artifact) error = %v, want a missing-file error", err)
	}
}

func TestVerifyChainRejectsAStreamEndingInsideAnEntryBody(t *testing.T) {
	t.Parallel()

	// A stream ending inside an ordinary file's body or inside FILES.json
	// propagates the reader's error rather than a chain verdict: truncation says
	// nothing about whether the documents agree.
	const overDeclared = 1 << 20

	// The control is the same entries declaring the sizes they really carry,
	// which is the default fixture and is accepted.
	if err := verifyChainFixture(t, chainSpec{entries: chainEntries()}); err != nil {
		t.Fatalf("VerifyChain(the same entries declaring their real sizes) error = %v, want no error", err)
	}

	oversizedFile := chainEntries()
	oversizedFile[0].declaredSize = overDeclared

	cases := []struct {
		name string
		spec chainSpec
	}{
		{name: "an ordinary file", spec: chainSpec{entries: oversizedFile}},
		{name: testFilesName, spec: chainSpec{entries: chainEntries(), filesDeclaredSize: overDeclared}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, tc.spec)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("VerifyChain(%s declaring %d bytes) error = %v, want the stream ending early", tc.name, overDeclared, err)
			}
		})
	}
}

// listingDocument renders a FILES.json carrying exactly the rows given, so a
// test can hand the reader a listing the fixture builder would never produce.
func listingDocument(rows ...string) []byte {
	return []byte(`{"files":[` + strings.Join(rows, ",") + `]}`)
}

// dirRowListing renders a listing of count directory rows, each naming a path
// no fixture archive carries, so how many rows it has is the only thing about
// it a reader can object to.
func dirRowListing(count int64) []byte {
	var buf bytes.Buffer
	// Around 32 bytes a row at the counts this is called with, so the document
	// is grown once rather than doubled a dozen times on the way up.
	buf.Grow(int(count)*32 + 16)
	buf.WriteString(`{"files":[`)
	for i := range count {
		if i > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"name":"d/%d","ftype":%q}`, i, testFtypeDir)
	}
	buf.WriteString(`]}`)
	return buf.Bytes()
}

func TestVerifyChainRejectsTooManyListedRows(t *testing.T) {
	t.Parallel()

	// helpers.FilesManifestMaxBytes bounds bytes, not rows, so rows are capped at
	// helpers.ArchiveMaxEntryCount. Every row names an absent directory and the
	// archive carries only the two documents, so nothing else can answer.
	cases := []struct {
		name    string
		rows    int64
		wantErr bool
	}{
		{name: "exactly the row cap", rows: helpers.ArchiveMaxEntryCount},
		{name: "one row past the cap", rows: helpers.ArchiveMaxEntryCount + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{mutateFiles: func([]byte) []byte { return dirRowListing(tc.rows) }}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want the chain sentinel", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainJudgesEveryRowOnItsOwnFields(t *testing.T) {
	t.Parallel()

	// json.Decoder.Decode does not zero its destination, so a hoisted row variable
	// would let a row inherit the previous one's fields; identical file bytes make
	// that inheritance an acceptance, which the refusing row catches.
	shared := []byte("# shared\n")
	digest := hexDigest(shared)
	entries := []chainEntry{
		{name: "README.md", content: shared},
		{name: "second.md", content: shared},
	}
	cases := []struct {
		name    string
		second  string
		wantErr bool
	}{
		{name: "the second row spelled out in full", second: listingFileRowJSON("second.md", digest)},
		{name: "the second row omitting ftype and digest", second: `{"name":"second.md"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: entries, mutateFiles: func([]byte) []byte {
				return listingDocument(listingFileRowJSON("README.md", digest), tc.second)
			}}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsContentAroundTheListing(t *testing.T) {
	t.Parallel()

	// The walk must consume the closing bracket and brace, then require EOF:
	// json.Unmarshal and Python's json.loads both refuse trailing content, so
	// accepting it would be a parser differential against the reference reader.
	cases := []struct {
		mutate  func([]byte) []byte
		name    string
		wantErr bool
	}{
		{name: "the document as rendered"},
		{
			name:    "garbage after the closing brace",
			mutate:  func(b []byte) []byte { return []byte(string(b) + " GARBAGE") },
			wantErr: true,
		},
		{
			name:    "garbage in place of the closing brace",
			mutate:  func(b []byte) []byte { return []byte(strings.TrimSuffix(string(b), "}") + " GARBAGE") },
			wantErr: true,
		},
		{
			name:    "the array truncated inside a row",
			mutate:  func([]byte) []byte { return []byte(`{"files":[{"name":`) },
			wantErr: true,
		},
		{
			name:    "the array never closed",
			mutate:  func([]byte) []byte { return []byte(`{"files":[`) },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: chainEntries(), mutateFiles: tc.mutate})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainRejectsASecondFilesKey(t *testing.T) {
	t.Parallel()

	// json.Unmarshal takes the last of two "files" keys and a streaming loop the
	// first, so a second key is refused. The second array is empty, so only an
	// explicit refusal fails under either reading.
	cases := []struct {
		mutate  func([]byte) []byte
		name    string
		wantErr bool
	}{
		{name: "one files key"},
		{
			name:    "a second files key",
			mutate:  func(b []byte) []byte { return []byte(strings.TrimSuffix(string(b), "}") + `,"files":[]}`) },
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := verifyChainFixture(t, chainSpec{entries: chainEntries(), mutateFiles: tc.mutate})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainAcceptsAListingWithNoFilesKey(t *testing.T) {
	t.Parallel()

	// Zero "files" keys stays accepted as an empty listing, and the reverse rule
	// then answers: an archive of only the two documents passes, one carrying
	// files is refused.
	cases := []struct {
		name    string
		entries []chainEntry
		wantErr bool
	}{
		{name: "an archive carrying only the two documents"},
		{name: "an archive carrying files as well", entries: chainEntries(), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{
				entries:     tc.entries,
				mutateFiles: func([]byte) []byte { return []byte(`{"format":1}`) },
			}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainSkipsUnknownListingKeys(t *testing.T) {
	t.Parallel()

	// A key this reader does not read, "format" among them, is skipped whole
	// wherever it sits; the last row pins that the skip consumes exactly the value.
	absent := listingFileRowJSON("absent.md", testOtherDigest)
	cases := []struct {
		name        string
		needle      string
		replacement string
		wantErr     bool
	}{
		{name: "a scalar before the array", needle: `{"files":[`, replacement: `{"format":1,"files":[`},
		{name: "a scalar after the array", needle: `]}`, replacement: `],"format":1}`},
		{
			name:   "a nested value before the array",
			needle: `{"files":[`, replacement: `{"extra":{"a":[1,2,{"b":null}],"c":"d"},"files":[`,
		},
		{
			name:   "a scalar ahead of a listing that still refuses",
			needle: `{"files":[`, replacement: `{"format":1,"files":[` + absent + `,`, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: chainEntries(), mutateFiles: replaceOnce(t, tc.needle, tc.replacement)}
			err := verifyChainFixture(t, spec)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

// fieldMessageCap bounds a refusal that reports a field's length: every such
// message is a sentence plus two numbers, so anything near this bound is the
// value having been rendered rather than described.
const fieldMessageCap = 200

// overlongFieldRow renders one directory row with field set to length bytes and
// the other fields ordinary, returning the row and the value it carries.
func overlongFieldRow(t *testing.T, field string, length int) (string, string) {
	t.Helper()

	value := strings.Repeat("a", length)
	fields := map[string]string{"name": "extra", "ftype": testFtypeDir}
	fields[field] = value
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("failed to render the fixture's row: %v", err)
	}
	return string(body), value
}

func TestVerifyChainRejectsOverlongListedFields(t *testing.T) {
	t.Parallel()

	// Every FILES.json string can reach a refusal, so each is capped at
	// helpers.ArchiveMaxEntryNameLen and the refusal reports the length, not the
	// value. Each field is paired at the cap and one byte over.
	cases := []struct {
		name    string
		field   string
		length  int
		wantErr bool
	}{
		{name: "name at the cap", field: "name", length: testEntryNameCap},
		{name: "name one byte over", field: "name", length: testEntryNameCap + 1, wantErr: true},
		{name: "ftype at the cap", field: "ftype", length: testEntryNameCap},
		{name: "ftype one byte over", field: "ftype", length: testEntryNameCap + 1, wantErr: true},
		{name: "chksum_type at the cap", field: "chksum_type", length: testEntryNameCap},
		{name: "chksum_type one byte over", field: "chksum_type", length: testEntryNameCap + 1, wantErr: true},
		{name: "chksum_sha256 at the cap", field: "chksum_sha256", length: testEntryNameCap},
		{name: "chksum_sha256 one byte over", field: "chksum_sha256", length: testEntryNameCap + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			row, value := overlongFieldRow(t, tc.field, tc.length)
			raw, manifestJSON := chainStreamFixture(t, chainSpec{
				entries:     chainEntries(),
				mutateFiles: replaceOnce(t, `{"files":[`, `{"files":[`+row+`,`),
			})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			checkBoundedRefusal(t, tc.name, err, value)
		})
	}
}

// checkBoundedRefusal asserts a refusal describes an over-long value rather
// than reproducing it: the value must be absent and the message within
// fieldMessageCap, since a rendered prefix would pass the first check alone.
func checkBoundedRefusal(t *testing.T, name string, err error, value string) {
	t.Helper()

	if strings.Contains(err.Error(), value) {
		t.Fatalf("verifyChainStream(%s) message reproduces the %d-byte value", name, len(value))
	}
	if got := len(err.Error()); got > fieldMessageCap {
		t.Fatalf("verifyChainStream(%s) message is %d bytes, want under %d", name, got, fieldMessageCap)
	}
}

func TestVerifyChainRejectsOverlongPointerFields(t *testing.T) {
	t.Parallel()

	// MANIFEST.json is bounded only by helpers.ManifestScanMaxBytes and a refusal
	// renders the pointer's strings with %q, so the same cap and length-not-value
	// rule apply. The control is the manifest as rendered.
	long := strings.Repeat("a", testEntryNameCap+1)
	cases := []struct {
		name    string
		needle  string
		digest  string
		wantErr bool
	}{
		{name: "the manifest as rendered"},
		{name: "name", needle: `"name":"` + testFilesName + `"`, wantErr: true},
		{name: "chksum_type", needle: `"chksum_type":"` + testChksumSHA256 + `"`, wantErr: true},
		{name: "chksum_sha256", digest: long, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := chainSpec{entries: chainEntries(), pointerDigest: tc.digest}
			if tc.needle != "" {
				field, _, _ := strings.Cut(tc.needle, ":")
				spec.mutateManifest = replaceOnce(t, tc.needle, field+`:"`+long+`"`)
			}
			raw, manifestJSON := chainStreamFixture(t, spec)
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			checkBoundedRefusal(t, tc.name, err, long)
		})
	}
}

func TestVerifyChainRejectsARegularFileListedAsSomethingElse(t *testing.T) {
	t.Parallel()

	// A non-file row over a regular file is refused: otherwise the path counts as
	// listed and its content, here a hostile script, is never checked. The control
	// lists it as the file it is.
	cases := []struct {
		name    string
		listAs  string
		wantErr bool
	}{
		{name: "listed as the file it is", listAs: testFtypeFile},
		{name: "listed as a directory", listAs: testFtypeDir, wantErr: true},
		{name: "listed under an ftype no reader knows", listAs: "wharrgarbl", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := chainEntries()
			entries[0].content = []byte("rm -rf /\n")
			entries[0].listAs = tc.listAs

			err := verifyChainFixture(t, chainSpec{entries: entries})
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("VerifyChain(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("VerifyChain(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

func TestVerifyChainCapsAnEntryNameBeforeAnythingRendersIt(t *testing.T) {
	t.Parallel()

	// chargeEntrySize quotes header.Name, so the name cap must run first or an
	// unmeasured name reaches its message. The second row shows the size rule
	// quoting an ordinary name, the third that the name rule answers first.
	const (
		longNameLen         = 200_000
		entryMessageCap     = 300
		oversizeDeclaration = helpers.ArchiveMaxEntrySize + 1
	)
	cases := []struct {
		want   error
		name   string
		entry  string
		quotes string
		size   int64
	}{
		{name: "the fixture as it stands"},
		{
			name: "an over-cap size under an ordinary name", entry: "huge.bin", size: oversizeDeclaration,
			want: helpers.ErrArchiveEntryIsTooLarge, quotes: "huge.bin",
		},
		{
			name: "a name past the cap", entry: strings.Repeat("a", longNameLen), size: oversizeDeclaration,
			want: helpers.ErrArchiveEntryNameTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := chainEntries()
			if tc.entry != "" {
				entries = append(entries, chainEntry{name: tc.entry, declaredSize: tc.size, unlisted: true})
			}
			raw, manifestJSON := chainStreamFixture(t, chainSpec{entries: entries})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if got := len(err.Error()); got > entryMessageCap {
				t.Fatalf("verifyChainStream(%s) message is %d bytes, want under %d", tc.name, got, entryMessageCap)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("verifyChainStream(%s) error = %v, want %v", tc.name, err, tc.want)
			}
			if tc.quotes != "" && !strings.Contains(err.Error(), tc.quotes) {
				t.Fatalf("verifyChainStream(%s) message does not name %q", tc.name, tc.quotes)
			}
		})
	}
}

func TestVerifyChainRejectsAListingOfTheWrongShape(t *testing.T) {
	t.Parallel()

	// expectDelim refuses a non-object document and a non-array "files", null
	// included, which json.Unmarshal would accept as empty and Python's json.loads
	// fails on. Only the two documents are archived, so nothing else answers.
	cases := []struct {
		name     string
		document string
		expects  string
	}{
		{name: "the listing as rendered"},
		{name: "a null document", document: `null`, expects: `"{" was expected`},
		{name: "an array document", document: `[]`, expects: `"{" was expected`},
		{name: "a number document", document: `42`, expects: `"{" was expected`},
		{name: "files as an object", document: `{"files":{}}`, expects: `"[" was expected`},
		{name: "files as null", document: `{"files":null}`, expects: `"[" was expected`},
		{name: "files as a number", document: `{"files":42}`, expects: `"[" was expected`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var spec chainSpec
			if tc.document != "" {
				spec.mutateFiles = func([]byte) []byte { return []byte(tc.document) }
			}
			raw, manifestJSON := chainStreamFixture(t, spec)
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.expects == "" {
				if err != nil {
					t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.expects) {
				t.Fatalf("verifyChainStream(%s) message does not carry %s", tc.name, tc.expects)
			}
		})
	}
}

func TestVerifyChainExemptsOnlyAFileRowForTheFilesManifest(t *testing.T) {
	t.Parallel()

	// The skip of FILES.json's own digest covers only a file row: a dir row for it
	// over the regular file is still refused, as for any path. The control is a
	// file row naming a digest the listing cannot have computed.
	cases := []struct {
		name    string
		row     string
		wantErr bool
	}{
		{name: "a file row naming a digest it cannot have computed", row: listingFileRowJSON(testFilesName, testOtherDigest)},
		{name: "a dir row", row: `{"name":"` + testFilesName + `","ftype":"` + testFtypeDir + `"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, manifestJSON := chainStreamFixture(t, chainSpec{
				entries:     chainEntries(),
				mutateFiles: replaceOnce(t, `{"files":[`, `{"files":[`+tc.row+`,`),
			})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s for %s) error = %v, want refusal", tc.name, testFilesName, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verifyChainStream(%s for %s) error = %v, want no error", tc.name, testFilesName, err)
			}
		})
	}
}

func TestVerifyChainRejectsATruncatedSkippedValue(t *testing.T) {
	t.Parallel()

	// skipListingValue must stop when the document ends: a value that never closes
	// leaves the decoder returning io.EOF, and ignoring that error spins forever.
	// Only the two documents are archived, so the reverse rule cannot answer.
	cases := []struct {
		name     string
		document string
		wantErr  bool
	}{
		{name: "a value that closes", document: `{"extra":[1,2],"files":[]}`},
		{name: "a value that runs out", document: `{"extra":[1,2`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, manifestJSON := chainStreamFixture(t, chainSpec{
				mutateFiles: func([]byte) []byte { return []byte(tc.document) },
			})
			err := verifyChainStream(t.Context(), bytes.NewReader(raw), manifestJSON, helpers.ArchiveMaxDecompressedSize)
			if tc.wantErr && !errors.Is(err, helpers.ErrManifestChainMismatch) {
				t.Fatalf("verifyChainStream(%s) error = %v, want refusal", tc.name, err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verifyChainStream(%s) error = %v, want no error", tc.name, err)
			}
		})
	}
}

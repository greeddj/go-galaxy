package manifest

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

// testManifest is every fixture's top-level MANIFEST.json; assertions compare
// its exact bytes, so a walk returning another entry's body fails on content.
const testManifest = `{"collection_info":{"namespace":"acme","name":"widgets","version":"1.0.0"}}`

// testEntry describes one tar entry for tarStream. declaredSize overrides the
// header's size (zero means the length of content); typeflag defaults to
// tar.TypeReg.
type testEntry struct {
	name, linkname string
	content        []byte
	declaredSize   int64
	typeflag       byte
}

// tarStream renders entries, in order, into a raw tar stream. Each entry has
// its own tar.Writer so a header may declare bytes it never carries; Flush then
// reports the shortfall, which is expected, and writes no padding.
func tarStream(tb testing.TB, entries []testEntry) []byte {
	tb.Helper()

	// A fixed stamp, so a fixture's bytes never depend on when it was built.
	modTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	for _, e := range entries {
		size := e.declaredSize
		if size == 0 {
			size = int64(len(e.content))
		}
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		tw := tar.NewWriter(&buf)
		header := &tar.Header{Typeflag: typeflag, Name: e.name, Linkname: e.linkname, Size: size, Mode: 0o644, ModTime: modTime}
		if err := tw.WriteHeader(header); err != nil {
			tb.Fatalf("failed to write the tar header for %s: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				tb.Fatalf("failed to write the tar content for %s: %v", e.name, err)
			}
		}
		_ = tw.Flush()
	}

	// The trailer: two zero blocks.
	buf.Write(make([]byte, 2*512))
	return buf.Bytes()
}

// writeArtifact gzips entries into a file under the test's own temp directory
// and returns its absolute path, which is the shape ReadFromTarGz takes.
func writeArtifact(tb testing.TB, entries []testEntry) string {
	tb.Helper()

	var buf bytes.Buffer
	gz := pgzip.NewWriter(&buf)
	if _, err := gz.Write(tarStream(tb, entries)); err != nil {
		tb.Fatalf("failed to compress the fixture: %v", err)
	}
	if err := gz.Close(); err != nil {
		tb.Fatalf("failed to close the fixture's gzip writer: %v", err)
	}
	return writeFile(tb, "artifact.tar.gz", buf.Bytes())
}

// writePaddedArtifact writes padBytes of zeros ahead of a top-level
// MANIFEST.json, compressed in chunks so a fixture past the real scan bound
// stays cheap in memory and on disk.
func writePaddedArtifact(t *testing.T, padBytes int64) string {
	t.Helper()

	var buf bytes.Buffer
	gz := pgzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	modTime := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	pad := &tar.Header{Typeflag: tar.TypeReg, Name: "pad.bin", Size: padBytes, Mode: 0o644, ModTime: modTime}
	if err := tw.WriteHeader(pad); err != nil {
		t.Fatalf("failed to write the padding header: %v", err)
	}
	chunk := make([]byte, 1<<20)
	for written := int64(0); written < padBytes; written += int64(len(chunk)) {
		if _, err := tw.Write(chunk[:min(int64(len(chunk)), padBytes-written)]); err != nil {
			t.Fatalf("failed to write the padding body: %v", err)
		}
	}

	manifest := &tar.Header{
		Typeflag: tar.TypeReg, Name: helpers.ManifestFileName,
		Size: int64(len(testManifest)), Mode: 0o644, ModTime: modTime,
	}
	if err := tw.WriteHeader(manifest); err != nil {
		t.Fatalf("failed to write the manifest header: %v", err)
	}
	if _, err := tw.Write([]byte(testManifest)); err != nil {
		t.Fatalf("failed to write the manifest body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("failed to close the fixture's tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close the fixture's gzip writer: %v", err)
	}
	return writeFile(t, "padded.tar.gz", buf.Bytes())
}

// writeFile drops data at name under the test's own temp directory and returns
// the absolute path.
func writeFile(tb testing.TB, name string, data []byte) string {
	tb.Helper()

	path := filepath.Join(tb.TempDir(), name)
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		tb.Fatalf("failed to write the fixture %s: %v", name, err)
	}
	return path
}

// nonManifestEntries is the surrounding content a real collection carries,
// returned fresh per call so a caller can append to it without aliasing.
func nonManifestEntries() []testEntry {
	return []testEntry{
		{name: "README.md", content: []byte("# acme.widgets\n")},
		{name: "roles/", typeflag: tar.TypeDir},
		{name: "plugins/modules/widget.py", content: []byte("# widget\n")},
	}
}

func TestReadFromTarGzFindsFirstEntry(t *testing.T) {
	t.Parallel()

	artifact := writeArtifact(t, append(
		[]testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}},
		nonManifestEntries()...))

	got, err := ReadFromTarGz(t.Context(), artifact)
	if err != nil {
		t.Fatalf("ReadFromTarGz(manifest first) error = %v, want the manifest", err)
	}
	if string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(manifest first) = %q, want %q", got, testManifest)
	}
}

func TestReadFromTarGzFindsLateEntry(t *testing.T) {
	t.Parallel()

	artifact := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)},
		testEntry{name: "FILES.json", content: []byte(`{"files":[]}`)}))

	got, err := ReadFromTarGz(t.Context(), artifact)
	if err != nil {
		t.Fatalf("ReadFromTarGz(manifest fourth) error = %v, want the manifest", err)
	}
	if string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(manifest fourth) = %q, want %q", got, testManifest)
	}
}

func TestReadFromTarGzRejectsMissingManifest(t *testing.T) {
	t.Parallel()

	// The positive control comes first: the identical entries plus a top-level
	// manifest are read back, so the refusal below is this walk finding no
	// manifest rather than the fixture being unreadable to begin with.
	control := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)}))
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(control with a manifest) = %q, %v, want the manifest and no error", got, err)
	}

	artifact := writeArtifact(t, nonManifestEntries())
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(no manifest) error = %v, want the not-found sentinel", err)
	}
}

func TestReadFromTarGzRejectsNestedManifest(t *testing.T) {
	t.Parallel()

	nested := testEntry{name: "vendored/other/" + helpers.ManifestFileName, content: []byte(`{"decoy":true}`)}

	// The control puts the nested entry ahead of the real one, so a check on the
	// base name alone would return the decoy.
	control := writeArtifact(t, []testEntry{
		nested,
		{name: helpers.ManifestFileName, content: []byte(testManifest)},
	})
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(nested entry ahead of the real one) = %q, want the top-level manifest (err: %v)", got, err)
	}

	artifact := writeArtifact(t, append(nonManifestEntries(), nested))
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(nested manifest only) error = %v, want the not-found sentinel", err)
	}
}

func TestReadFromTarGzRejectsOversizeEntry(t *testing.T) {
	t.Parallel()

	// One byte past the per-entry cap and carrying nothing: the walk must refuse
	// it on the header alone, before reading a body that size.
	oversize := testEntry{name: "huge.bin", declaredSize: helpers.ArchiveMaxEntrySize + 1}
	manifest := testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)}

	// The positive control is the same stream with the over-declared entry
	// left out, so the refusal below is the size check answering rather than
	// the manifest sitting somewhere this walk never looks.
	control := writeArtifact(t, []testEntry{{name: "small.bin", content: []byte("x")}, manifest})
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(control without the over-declared entry) = %q, %v, want the manifest", got, err)
	}

	artifact := writeArtifact(t, []testEntry{oversize, manifest})
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrArchiveEntryIsTooLarge) {
		t.Fatalf("ReadFromTarGz(over-declared entry ahead of the manifest) error = %v, want the entry-size sentinel", err)
	}
}

func TestReadFromTarGzRejectsScanOverrun(t *testing.T) {
	t.Parallel()

	// Both fixtures carry a real MANIFEST.json behind the padding, so the refusal
	// means "stopped at the ceiling", not "read everything and found nothing".
	beyond := writePaddedArtifact(t, helpers.ManifestScanMaxBytes+(1<<20))
	if _, err := ReadFromTarGz(t.Context(), beyond); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(manifest past the scan bound) error = %v, want the not-found sentinel", err)
	}

	// The control sits just under the same ceiling, so the refusal above is
	// the bound answering rather than padding this walk cannot read through.
	within := writePaddedArtifact(t, helpers.ManifestScanMaxBytes-(1<<20))
	got, err := ReadFromTarGz(t.Context(), within)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(manifest just inside the scan bound) = %q, %v, want the manifest", got, err)
	}
}

func TestReadFromTarGzRejectsNonGzip(t *testing.T) {
	t.Parallel()

	// The positive control proves a path handed to this function is read at
	// all, so the two refusals below are the gzip header answering rather than
	// anything about how the fixture reached disk.
	control := writeArtifact(t, []testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}})
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(control gzipped artifact) = %q, %v, want the manifest", got, err)
	}

	text := writeFile(t, "error-page.html", []byte("<html><body>404 Not Found</body></html>"))
	if _, err := ReadFromTarGz(t.Context(), text); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ReadFromTarGz(an error page) error = %v, want the shape sentinel", err)
	}

	// A bare tar, never compressed: a well-formed archive of the right inner
	// shape that is still not what this function reads.
	bare := writeFile(t, "bare.tar", tarStream(t, []testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}}))
	if _, err := ReadFromTarGz(t.Context(), bare); !errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ReadFromTarGz(an uncompressed tar) error = %v, want the shape sentinel", err)
	}
}

func TestReadFromTarGzRejectsEmptyManifest(t *testing.T) {
	t.Parallel()

	// A one-byte manifest is the positive control, and it is the tightest one
	// available: the entry sits at the same position under the same name, so
	// only its length separates it from the refusal below.
	control := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName, content: []byte("{")}))
	got, err := ReadFromTarGz(t.Context(), control)
	if err != nil || string(got) != "{" {
		t.Fatalf("ReadFromTarGz(one-byte manifest) = %q, %v, want that one byte", got, err)
	}

	// A signature over a zero-byte document verifies whenever a keyring key made
	// it, so an empty manifest is refused here, where it is still recognizable.
	artifact := writeArtifact(t, append(nonManifestEntries(),
		testEntry{name: helpers.ManifestFileName}))
	if _, err := ReadFromTarGz(t.Context(), artifact); !errors.Is(err, helpers.ErrManifestNotFound) {
		t.Fatalf("ReadFromTarGz(zero-length manifest) error = %v, want the not-found sentinel", err)
	}
}

func TestReadFromTarGzNeverReportsAnEmptyManifest(t *testing.T) {
	t.Parallel()

	// A header declaring more than the archive carries, in the two shapes that
	// differ. Both are pinned because only one of them errors, and the contract
	// - a nil error never comes with no bytes - has to hold across both.
	const declared = 100
	body := []byte("0123456789")

	// The archive's own trailer sits behind the short body, so the read is
	// satisfied from it: exactly the declared count, padded with NUL.
	padded := writeArtifact(t, []testEntry{
		{name: helpers.ManifestFileName, content: body, declaredSize: declared},
	})
	got, err := ReadFromTarGz(t.Context(), padded)
	if err != nil || len(got) != declared {
		t.Fatalf("ReadFromTarGz(header over a short body) = %d bytes, %v, want %d bytes and no error", len(got), err, declared)
	}

	// The same header with the stream ending inside the body instead: nothing
	// follows it, not even a trailer, so archive/tar has nothing to satisfy the
	// declaration from.
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	header := &tar.Header{Typeflag: tar.TypeReg, Name: helpers.ManifestFileName, Size: declared, Mode: 0o644}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("failed to write the truncated fixture's header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("failed to write the truncated fixture's body: %v", err)
	}

	var compressed bytes.Buffer
	gz := pgzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatalf("failed to compress the truncated fixture: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close the truncated fixture's gzip writer: %v", err)
	}

	truncated := writeFile(t, "truncated.tar.gz", compressed.Bytes())
	got, err = ReadFromTarGz(t.Context(), truncated)
	if err == nil || len(got) != 0 {
		t.Fatalf("ReadFromTarGz(stream ending inside the manifest) = %d bytes, %v, want no bytes and an error", len(got), err)
	}
}

func TestReadFromTarGzRefusesAnArtifactItCannotOpen(t *testing.T) {
	t.Parallel()

	// The path is the run's own, so a missing file means an artifact evicted or
	// never written; the control is the same fixture where it really sits.
	artifact := writeArtifact(t, []testEntry{{name: helpers.ManifestFileName, content: []byte(testManifest)}})
	got, err := ReadFromTarGz(t.Context(), artifact)
	if err != nil || string(got) != testManifest {
		t.Fatalf("ReadFromTarGz(the fixture where it sits) = %q, %v, want the manifest", got, err)
	}

	if _, err := ReadFromTarGz(t.Context(), artifact+".absent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFromTarGz(a path carrying no artifact) error = %v, want a missing-file error", err)
	}
}

// errTestCeiling stands in for whichever verdict a caller injects, so the test
// below is about the reader rather than about either sentinel this package
// hands it.
var errTestCeiling = errors.New("test ceiling")

// countingReader answers every read with one byte and counts the calls it was
// handed, which is what tells a reader that stopped touching its source apart
// from one that merely keeps reporting the same refusal.
type countingReader struct {
	reads int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads++
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x'
	return 1, nil
}

func TestLimitReaderStaysRefusedPastItsCeiling(t *testing.T) {
	t.Parallel()

	// The source is endless, so only the ceiling ends this. Stickiness shows only
	// in the source's call count: a reader recomputing the refusal would still
	// pull a byte out of the decompressor on every call.
	source := &countingReader{}
	limited := &limitReader{r: source, over: errTestCeiling, max: 2}
	buf := make([]byte, 8)

	for read := range 2 {
		if n, err := limited.Read(buf); n != 1 || err != nil {
			t.Fatalf("limitReader.Read under the ceiling = %d, %v on read %d, want one byte and no error", n, err, read)
		}
	}

	n, crossing := limited.Read(buf)
	if n != 0 || !errors.Is(crossing, errTestCeiling) {
		t.Fatalf("limitReader.Read crossing the ceiling = %d, %v, want no bytes and the injected verdict", n, crossing)
	}
	crossed := source.reads

	n, again := limited.Read(buf)
	if n != 0 || !errors.Is(again, crossing) || source.reads != crossed {
		t.Fatalf("limitReader.Read past the ceiling = %d, %v after %d source reads, want %d", n, again, source.reads, crossed)
	}
}

func TestReadFromTarGzCapsAnEntryNameBeforeAnythingRendersIt(t *testing.T) {
	t.Parallel()

	// The size refusal quotes the entry name and archive/tar accepts GNU long
	// names near a megabyte, so the name cap must answer first; %q quoting keeps
	// a newline in a name, which safeout.Clean passes, from forging output lines.
	const (
		longNameLen         = 200_000
		oversizeDeclaration = helpers.ArchiveMaxEntrySize + 1
	)
	cases := []nameCapCase{
		{name: "the fixture as it stands"},
		{name: "a name at the cap", entry: strings.Repeat("a", testEntryNameCap)},
		{
			name: "a name one byte over the cap", entry: strings.Repeat("a", testEntryNameCap+1),
			want: helpers.ErrArchiveEntryNameTooLong,
		},
		{
			name: "an over-cap size under an ordinary name", entry: "huge.bin", size: oversizeDeclaration,
			want: helpers.ErrArchiveEntryIsTooLarge, quotes: `"huge.bin"`,
		},
		{
			name: "a name past the cap under an over-cap size", size: oversizeDeclaration,
			entry: strings.Repeat("a", longNameLen), want: helpers.ErrArchiveEntryNameTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entries := nonManifestEntries()
			if tc.entry != "" {
				entries = append(entries, testEntry{name: tc.entry, declaredSize: tc.size})
			}
			entries = append(entries, testEntry{name: helpers.ManifestFileName, content: []byte(testManifest)})

			got, err := artifactStream(t, entries)
			if tc.want == nil {
				if err != nil || string(got) != testManifest {
					t.Fatalf("readFromTarGzStream(%s) = %q, %v, want the manifest", tc.name, got, err)
				}
				return
			}
			checkNameCapRefusal(t, tc, err)
		})
	}
}

// nameCapCase is one row of the name-cap table: an entry put ahead of the
// manifest and the refusal it owes, with quotes naming a fragment it renders.
// A nil want marks an accepting row.
type nameCapCase struct {
	want   error
	name   string
	entry  string
	quotes string
	size   int64
}

// checkNameCapRefusal asserts a refusing row's message length, sentinel and
// quoted fragment. The length is checked first so a failure never renders a
// 200,000-byte name into the test log.
func checkNameCapRefusal(t *testing.T, tc nameCapCase, err error) {
	t.Helper()

	const messageMax = 300
	if err == nil {
		t.Fatalf("readFromTarGzStream(%s) error = <nil>, want %v", tc.name, tc.want)
	}
	if n := len(err.Error()); n > messageMax {
		t.Fatalf("readFromTarGzStream(%s) message is %d bytes, want under %d", tc.name, n, messageMax)
	}
	if !errors.Is(err, tc.want) {
		t.Fatalf("readFromTarGzStream(%s) error = %v, want %v", tc.name, err, tc.want)
	}
	if tc.quotes != "" && !strings.Contains(err.Error(), tc.quotes) {
		t.Fatalf("readFromTarGzStream(%s) message does not quote %s", tc.name, tc.quotes)
	}
}

// artifactStream runs the walk over a fixture without going through
// ReadFromTarGz, so an assertion about the length of a refusal measures what
// the walk renders rather than the artifact path ReadFromTarGz prefixes on.
func artifactStream(tb testing.TB, entries []testEntry) ([]byte, error) {
	tb.Helper()

	raw, err := os.ReadFile(writeArtifact(tb, entries))
	if err != nil {
		tb.Fatalf("failed to read the fixture back: %v", err)
	}
	return readFromTarGzStream(tb.Context(), bytes.NewReader(raw))
}

// TestReadFromTarGzRefusesAGzipMemberProducingNoBytes pins that an empty but
// well-formed gzip member opens, and is refused by the walk with
// helpers.ErrEmptyGzipMember rather than by the open as a shape failure.
func TestReadFromTarGzRefusesAGzipMemberProducingNoBytes(t *testing.T) {
	t.Parallel()

	// Twenty hand-spelled bytes form a whole gzip member: a ten-byte header, an
	// empty fixed-Huffman final block and an eight-byte trailer over nothing.
	member := make([]byte, 0, 20)
	member = append(member, "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"...)
	member = append(member, 0x03, 0x00)
	member = append(member, 0, 0, 0, 0, 0, 0, 0, 0)

	_, err := ReadFromTarGz(t.Context(), writeFile(t, "empty.tar.gz", member))

	// gzipstream's member rule answers; without it the walk would report a
	// stream that simply ended with no manifest.
	if !errors.Is(err, helpers.ErrEmptyGzipMember) {
		t.Fatalf("ReadFromTarGz(an empty gzip member) error = %v, want the empty-member sentinel", err)
	}

	// The verdict is the walk's rather than the open's, so it carries no shape
	// sentinel.
	if errors.Is(err, helpers.ErrArtifactNotTarGz) {
		t.Fatalf("ReadFromTarGz(an empty gzip member) error = %v, want no shape sentinel", err)
	}
}

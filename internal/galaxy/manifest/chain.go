package manifest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
)

const (
	// chainHashBufSize is the copy buffer one scan reuses for every regular
	// file it digests, so an archive of any size costs one buffer rather than
	// one per entry.
	chainHashBufSize = 64 << 10
	// chainMaxLinkHops bounds how far a listed name is followed through link
	// entries. Python's tarfile resolves a link to a link, so refusing the second
	// hop would reject archives it reads; the bound is also the cycle guard.
	chainMaxLinkHops = 8
	// filesEntryTypeFile is the ftype FILES.json gives a row whose digest
	// describes file content. Every other ftype - "dir", most commonly - is
	// recorded as listed and not digested.
	filesEntryTypeFile = "file"
	// filesListingKey is the one key of FILES.json this reader reads: the array
	// of rows describing what the archive carries.
	filesListingKey = "files"
	// chksumTypeSHA256 is the one checksum algorithm named in these documents
	// that this reader verifies.
	chksumTypeSHA256 = "sha256"
)

// Nibble arithmetic for sha256FromHex, named rather than spelled inline: one
// digest byte is two hex digits, the first of which carries the high four bits,
// and a letter digit's value starts where the decimal digits end.
const (
	hexDigitsPerByte   = 2
	hexHighNibbleShift = 4
	hexLetterValue     = 10
)

// VerifyChain checks that the artifact at artifactPath carries exactly what
// manifestJSON, already signature-verified, vouches for through FILES.json. The
// tar stream stays attacker-chosen, so it is bounded as archive.ExtractTarGz is.
func VerifyChain(ctx context.Context, artifactPath string, manifestJSON []byte) error {
	//nolint:gosec // artifactPath names an artifact this run downloaded or produced.
	file, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("failed to open artifact to verify its manifest chain: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	if err := verifyChainStream(ctx, file, manifestJSON, helpers.ArchiveMaxDecompressedSize); err != nil {
		return fmt.Errorf("%s: %w", artifactPath, err)
	}
	return nil
}

// verifyChainStream is VerifyChain over an open reader with the decompressed
// cap injected, so a test needs no production-sized fixture. It parses the
// pointer before reading the archive, and it does not close r.
func verifyChainStream(ctx context.Context, r io.Reader, manifestJSON []byte, maxDecompressed int64) error {
	wantFiles, err := parseChainPointer(manifestJSON)
	if err != nil {
		return err
	}

	uncompressed, err := gzipstream.NewReader(ctx, r)
	if err != nil {
		return decompressorOpenError(err)
	}
	defer func() {
		_ = uncompressed.Close()
	}()

	limited := &limitReader{r: uncompressed, over: helpers.ErrArchiveDecompressedTooLarge, max: maxDecompressed}
	scan, err := scanArchive(tar.NewReader(&contextReader{ctx: ctx, r: limited}))
	if err != nil {
		return err
	}
	return verifyChainScan(scan, wantFiles, manifestJSON)
}

// contextReader fails a read once ctx is done and returns ctx.Err() unwrapped,
// so cmd/go-galaxy/exitcode can classify cancellation through errors.Is.
//
//nolint:containedctx // io.Reader cannot take a context per call
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// chainPointer is the one field of MANIFEST.json this package reads. It is a
// local type, not the vendored hub one, because a renamed json tag in a
// dependency bump would silently turn the chain check into a no-op.
type chainPointer struct {
	FileManifestFile struct {
		Name         string `json:"name"`
		ChksumType   string `json:"chksum_type"`
		ChksumSha256 string `json:"chksum_sha256"`
	} `json:"file_manifest_file"`
}

// filesEntry is one row of FILES.json.
type filesEntry struct {
	Name         string `json:"name"`
	Ftype        string `json:"ftype"`
	ChksumType   string `json:"chksum_type"`
	ChksumSha256 string `json:"chksum_sha256"`
}

// archiveScan is what one pass over the tar stream retains. order keeps the
// arrival order, so the unlisted-entry refusal names the same path every run.
type archiveScan struct {
	hashes map[string][32]byte
	links  map[string]string
	files  []byte
	order  []string
}

// chainScanner is the per-call state a scan reuses across entries: the hash,
// the copy buffer it digests through, and the scratch the digest is summed
// into. None of the three may be allocated per entry.
type chainScanner struct {
	scan    *archiveScan
	digest  hash.Hash
	buf     []byte
	scratch [sha256.Size]byte
}

// scanArchive walks the tar stream once, digesting regular files, recording
// link targets and keeping FILES.json's bytes. Nothing is compared until EOF,
// so FILES.json may sit anywhere in the stream.
func scanArchive(tarReader *tar.Reader) (*archiveScan, error) {
	scanner := &chainScanner{
		scan:   &archiveScan{hashes: make(map[string][32]byte)},
		digest: sha256.New(),
		buf:    make([]byte, chainHashBufSize),
	}

	var declared, entries int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return scanner.scan, nil
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read the artifact's tar stream: %w", err)
		}
		entries++
		if entries > helpers.ArchiveMaxEntryCount {
			return nil, fmt.Errorf("%w: %d", helpers.ErrArchiveTooManyEntries, helpers.ArchiveMaxEntryCount)
		}
		// Ahead of chargeEntrySize, whose refusals quote header.Name and must
		// never render a name nothing has measured.
		if err := checkEntryNameLengths(header); err != nil {
			return nil, err
		}
		if err := chargeEntrySize(header, &declared); err != nil {
			return nil, err
		}
		if err := scanner.entry(tarReader, header); err != nil {
			return nil, err
		}
	}
}

// chargeEntrySize charges header.Size against the per-entry and per-archive
// byte budgets. scanArchive calls it on every header before the typeflag
// dispatch, so a skipped entry costs what a digested one does.
func chargeEntrySize(header *tar.Header, declared *int64) error {
	if header.Size < 0 {
		return fmt.Errorf("%w: %q", helpers.ErrArchiveEntryHasNegativeSize, header.Name)
	}
	if header.Size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w %q: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
	}
	if *declared+header.Size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	*declared += header.Size
	return nil
}

// entry records one tar entry into the scan. Only regular files and links are
// kept: a directory has no content to digest and its listing row no digest.
func (s *chainScanner) entry(tarReader *tar.Reader, header *tar.Header) error {
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
	default:
		return nil
	}
	key, err := cleanEntryPath(header.Name)
	if err != nil {
		return err
	}
	if key == "" {
		// The entry's name normalized away to the archive root, which names no
		// file. Skipped rather than refused, matching the extractor's own
		// handling of the same shape.
		return nil
	}
	if err := s.scan.claim(key); err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeSymlink:
		return s.scan.recordSymlink(key, header.Linkname)
	case tar.TypeLink:
		return s.scan.recordHardlink(key, header.Linkname)
	default:
		return s.recordRegular(tarReader, header, key)
	}
}

// checkEntryNameLength refuses a tar entry name past
// helpers.ArchiveMaxEntryNameLen, reporting its length rather than the name so
// a refusal never echoes megabytes of archive-chosen bytes.
func checkEntryNameLength(name string) error {
	if len(name) <= helpers.ArchiveMaxEntryNameLen {
		return nil
	}
	return fmt.Errorf("%w: an entry names itself in %d bytes, the limit is %d",
		helpers.ErrArchiveEntryNameTooLong, len(name), helpers.ArchiveMaxEntryNameLen)
}

// checkEntryNameLengths refuses an entry whose name or link target exceeds
// helpers.ArchiveMaxEntryNameLen. It runs on every header ahead of every rule
// that renders a name, so no message carries an unmeasured name.
func checkEntryNameLengths(header *tar.Header) error {
	if err := checkEntryNameLength(header.Name); err != nil {
		return err
	}
	if header.Typeflag != tar.TypeSymlink && header.Typeflag != tar.TypeLink {
		return nil
	}
	if len(header.Linkname) > helpers.ArchiveMaxEntryNameLen {
		return fmt.Errorf("%w: the link target of %q is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryNameTooLong, header.Name, len(header.Linkname), helpers.ArchiveMaxEntryNameLen)
	}
	return nil
}

// cleanEntryPath normalizes a tar entry name into its scan key or refuses it;
// "" with a nil error means the archive root. It uses path, not filepath: a tar
// name is slash-separated on every platform, so a backslash is ordinary.
func cleanEntryPath(name string) (string, error) {
	if name == "" {
		return "", helpers.ErrArchiveEntryHasEmptyName
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return "", nil
	}
	if path.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: %q", helpers.ErrArchiveEntryIsAbsolutePath, name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q", helpers.ErrArchiveEntryEscapesDestination, name)
	}
	return cleaned, nil
}

// claim reserves key for one entry and records its order. Refusing a duplicate
// keeps an archive from showing ReadFromTarGz, which takes the first match, a
// different MANIFEST.json than the one this chain walk hashes.
func (a *archiveScan) claim(key string) error {
	_, hashed := a.hashes[key]
	_, linked := a.links[key]
	if hashed || linked {
		return fmt.Errorf("%w: %q", helpers.ErrArchiveDuplicateEntry, key)
	}
	a.order = append(a.order, key)
	return nil
}

// recordSymlink resolves a symlink entry's target against the entry's own
// directory and records it, deferring the lookup until the whole stream has
// been read - the target may not have arrived yet.
func (a *archiveScan) recordSymlink(key, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("%w for %q", helpers.ErrSymlinkTargetIsEmpty, key)
	}
	// Tested on the raw value, before any join. path.Join("a", "/etc/passwd")
	// yields "a/etc/passwd", so an absolute target checked after the join has
	// already been silently rewritten into an in-archive one and would pass.
	if path.IsAbs(linkname) {
		return fmt.Errorf("%w: %q", helpers.ErrSymlinkTargetIsAbsolute, linkname)
	}
	resolved := path.Join(path.Dir(key), linkname)
	if resolved == "." || resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: %q", helpers.ErrSymlinkTargetEscapesDestination, linkname)
	}
	a.link(key, resolved)
	return nil
}

// recordHardlink records a hardlink entry's target, resolved against the
// archive root rather than the entry's directory, as both the extractor and
// Python's tarfile read it.
func (a *archiveScan) recordHardlink(key, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("%w for %q", helpers.ErrHardlinkTargetIsEmpty, key)
	}
	target, err := cleanEntryPath(linkname)
	if err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("%w for %q", helpers.ErrHardlinkTargetIsEmpty, key)
	}
	a.link(key, target)
	return nil
}

// link records one resolved link, allocating the map on first use: a collection
// carrying no links pays nothing for the ones it does not have.
func (a *archiveScan) link(key, target string) {
	if a.links == nil {
		a.links = make(map[string]string)
	}
	a.links[key] = target
}

// recordRegular digests one regular file into the scan, routing FILES.json to
// the arm that keeps its bytes.
func (s *chainScanner) recordRegular(tarReader *tar.Reader, header *tar.Header, key string) error {
	if key == helpers.FilesManifestFileName {
		return s.recordFilesManifest(tarReader, header.Size)
	}

	// CopyBuffer uses s.buf only while tar.Reader implements no io.WriterTo
	// and the sha256 digest no io.ReaderFrom; otherwise it would allocate its
	// own buffer for every entry.
	s.digest.Reset()
	if _, err := io.CopyBuffer(s.digest, tarReader, s.buf); err != nil {
		return fmt.Errorf("failed to read the artifact's tar stream: %w", err)
	}
	// Summed into a reusable array rather than onto a fresh nil slice, which
	// costs 32 B and one allocation per entry.
	s.scan.hashes[key] = [sha256.Size]byte(s.digest.Sum(s.scratch[:0]))
	return nil
}

// recordFilesManifest reads FILES.json whole for decoding, refusing a declared
// size past helpers.FilesManifestMaxBytes before that size drives the
// allocation.
func (s *chainScanner) recordFilesManifest(tarReader *tar.Reader, size int64) error {
	if size > helpers.FilesManifestMaxBytes {
		return fmt.Errorf("%w %s: %d bytes", helpers.ErrArchiveEntryIsTooLarge, helpers.FilesManifestFileName, size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(tarReader, body); err != nil {
		return fmt.Errorf("failed to read the artifact's tar stream: %w", err)
	}
	s.scan.files = body
	s.scan.hashes[helpers.FilesManifestFileName] = sha256.Sum256(body)
	return nil
}

// parseChainPointer reads the digest MANIFEST.json names for FILES.json.
// chksum_type is not checked: a value that is not a sha256 is refused by the
// digest shape check instead.
func parseChainPointer(manifestJSON []byte) ([32]byte, error) {
	// Any decode error fails closed: a type error still fills every field
	// the decoder could, so a manifest with a numeric chksum_type would
	// otherwise pass with a well-formed pointer.
	var doc chainPointer
	if err := json.Unmarshal(manifestJSON, &doc); err != nil {
		return [32]byte{}, fmt.Errorf("%w: %s does not parse: %w",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName, err)
	}
	if err := checkPointerFieldLengths(&doc); err != nil {
		return [32]byte{}, err
	}
	if path.Clean(doc.FileManifestFile.Name) != helpers.FilesManifestFileName {
		return [32]byte{}, fmt.Errorf("%w: %s points at %q rather than at %s",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName, doc.FileManifestFile.Name, helpers.FilesManifestFileName)
	}
	// Refused here so a manifest naming no digest is answered before any
	// byte is decompressed, which TestVerifyChainReadsThePointerBeforeTheArchive
	// pins.
	want, ok := sha256FromHex(doc.FileManifestFile.ChksumSha256)
	if !ok {
		return [32]byte{}, fmt.Errorf("%w: %s names %q as the digest of %s, which is not a sha256",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName,
			doc.FileManifestFile.ChksumSha256, helpers.FilesManifestFileName)
	}
	return want, nil
}

// verifyChainScan checks the scan against the signed manifest in order: the
// archive's MANIFEST.json is the verified one, FILES.json matches its pointer,
// every listed file matches, and nothing unlisted is carried.
func verifyChainScan(scan *archiveScan, wantFiles [32]byte, manifestJSON []byte) error {
	if got, ok := scan.hashes[helpers.ManifestFileName]; !ok || got != sha256.Sum256(manifestJSON) {
		return fmt.Errorf("%w: the archive's %s is not the document that was verified",
			helpers.ErrManifestChainMismatch, helpers.ManifestFileName)
	}
	// Not a verdict of its own - the checks below refuse a missing entry
	// anyway - but this message is the actionable one.
	gotFiles, ok := scan.hashes[helpers.FilesManifestFileName]
	if !ok {
		return fmt.Errorf("%w: the archive carries no regular-file %s",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName)
	}
	if gotFiles != wantFiles {
		return fmt.Errorf("%w: %s does not match the digest %s names for it",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, helpers.ManifestFileName)
	}

	listed, err := verifyListing(scan)
	if err != nil {
		return err
	}
	return checkUnlisted(scan, listed)
}

// verifyListing walks FILES.json against the archive and returns every name it
// lists, whatever the ftype. Rows are streamed: decoded whole, a capped listing
// of empty rows costs over a GiB of heap.
func verifyListing(scan *archiveScan) (map[string]struct{}, error) {
	listed := make(map[string]struct{}, len(scan.order))
	if err := walkListingDocument(json.NewDecoder(bytes.NewReader(scan.files)), scan, listed); err != nil {
		return nil, err
	}
	return listed, nil
}

// walkListingDocument reads FILES.json's top-level object, skipping unknown
// keys. A second "files" key and trailing content are refused, since
// json.Unmarshal or Python's json.loads would read such a document differently.
func walkListingDocument(dec *json.Decoder, scan *archiveScan, listed map[string]struct{}) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	var walked bool
	for {
		tok, err := dec.Token()
		if err != nil {
			return listingParseError(err)
		}
		key, ok := tok.(string)
		if !ok {
			// The brace closing the object: a key is a string by construction,
			// so nothing else can stand in this position.
			break
		}
		if key != filesListingKey {
			if err := skipListingValue(dec); err != nil {
				return err
			}
			continue
		}
		if walked {
			return fmt.Errorf("%w: %s carries more than one %q key",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, filesListingKey)
		}
		walked = true
		if err := walkListingRows(dec, scan, listed); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %s carries content after its top-level object",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName)
	}
	return nil
}

// walkListingRows reads the "files" array one row at a time, capped at
// helpers.ArchiveMaxEntryCount rows. The "." root row makes a listing for an
// archive at exactly that entry ceiling one row too many; that is accepted.
func walkListingRows(dec *json.Decoder, scan *archiveScan, listed map[string]struct{}) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	var rows int64
	for dec.More() {
		rows++
		if rows > helpers.ArchiveMaxEntryCount {
			return fmt.Errorf("%w: %s lists more than %d entries",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, helpers.ArchiveMaxEntryCount)
		}
		// Declared per row: Decode does not zero its destination, so a hoisted
		// row omitting ftype would inherit the previous row's and skip its
		// digest check.
		var row filesEntry
		if err := dec.Decode(&row); err != nil {
			// A type error is returned too: the decoder fills what it could
			// first, so the row would otherwise be judged on partial fields.
			return listingParseError(err)
		}
		if err := checkListedRow(scan, listed, &row); err != nil {
			return err
		}
	}
	return expectDelim(dec, ']')
}

// checkListedRow checks one FILES.json row and marks its name listed. A non-file
// row over a regular-file entry is refused, or its content would go unchecked;
// a link under such a row, like a symlink to a directory, is never resolved.
func checkListedRow(scan *archiveScan, listed map[string]struct{}, row *filesEntry) error {
	if err := checkListedFieldLengths(row); err != nil {
		return err
	}
	key, err := listedName(row.Name)
	if err != nil {
		return err
	}
	if _, dup := listed[key]; dup {
		return fmt.Errorf("%w: %s lists %q twice",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key)
	}
	listed[key] = struct{}{}

	if row.Ftype != filesEntryTypeFile {
		if _, isFile := scan.hashes[key]; isFile {
			return fmt.Errorf("%w: %s gives %q the type %q while the archive carries a regular file there",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key, row.Ftype)
		}
		return nil
	}
	// FILES.json's own file row is not digested: a listing cannot state its
	// own digest, and the signed MANIFEST.json already pins those bytes.
	if key == helpers.FilesManifestFileName {
		return nil
	}
	return verifyListedFile(scan, key, row)
}

// expectDelim reads one token and requires it to be want. The offending token
// is not rendered, since nothing has length-checked it yet.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return listingParseError(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != want {
		return fmt.Errorf("%w: %s does not parse: %q was expected",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, want)
	}
	return nil
}

// skipListingValue consumes the value under a key this reader does not read,
// whatever its shape, without retaining it as a json.RawMessage would.
func skipListingValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return listingParseError(err)
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			// A scalar, which is the whole value when nothing has opened.
			if depth == 0 {
				return nil
			}
			continue
		}
		if delim == '{' || delim == '[' {
			depth++
			continue
		}
		depth--
		if depth == 0 {
			return nil
		}
	}
}

// listingParseError wraps a decoder failure over FILES.json. Wrapping is safe:
// a syntax error renders one character and a type error names a JSON kind,
// never the value.
func listingParseError(err error) error {
	return fmt.Errorf("%w: %s does not parse: %w",
		helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, err)
}

// checkListedFieldLengths caps every string one FILES.json row can carry, all
// at one uniform limit.
func checkListedFieldLengths(row *filesEntry) error {
	if err := checkFieldLength(helpers.FilesManifestFileName, "name", row.Name); err != nil {
		return err
	}
	if err := checkFieldLength(helpers.FilesManifestFileName, "ftype", row.Ftype); err != nil {
		return err
	}
	if err := checkFieldLength(helpers.FilesManifestFileName, "chksum_type", row.ChksumType); err != nil {
		return err
	}
	return checkFieldLength(helpers.FilesManifestFileName, "chksum_sha256", row.ChksumSha256)
}

// checkPointerFieldLengths caps the three strings MANIFEST.json's pointer can
// carry, since a refusal renders each with %q. It bounds that rendering, not
// the decode, which has already happened.
func checkPointerFieldLengths(doc *chainPointer) error {
	if err := checkFieldLength(helpers.ManifestFileName, "name", doc.FileManifestFile.Name); err != nil {
		return err
	}
	if err := checkFieldLength(helpers.ManifestFileName, "chksum_type", doc.FileManifestFile.ChksumType); err != nil {
		return err
	}
	return checkFieldLength(helpers.ManifestFileName, "chksum_sha256", doc.FileManifestFile.ChksumSha256)
}

// checkFieldLength refuses a JSON string field past
// helpers.ArchiveMaxEntryNameLen, reporting its length rather than its value,
// since the document is attacker-chosen whenever the archive is.
func checkFieldLength(document, field, value string) error {
	if len(value) <= helpers.ArchiveMaxEntryNameLen {
		return nil
	}
	return fmt.Errorf("%w: %s carries a %s field of %d bytes, the limit is %d",
		helpers.ErrManifestChainMismatch, document, field, len(value), helpers.ArchiveMaxEntryNameLen)
}

// listedName normalizes a listed name into a scan key, refusing one outside the
// archive with ErrManifestChainMismatch: the fault is the listing's, not the
// archive's. A "." names the collection root and is kept.
func listedName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: %s lists an entry with no name",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName)
	}
	cleaned := path.Clean(name)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %s lists %q, which is not a path inside the archive",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, name)
	}
	return cleaned, nil
}

// verifyListedFile checks one listed file row against what the archive carries
// under that name. The row is taken by pointer rather than by value only to
// keep a 64-byte copy off a path that runs once per listed row.
func verifyListedFile(scan *archiveScan, key string, row *filesEntry) error {
	if row.ChksumType != chksumTypeSHA256 {
		return fmt.Errorf("%w: %s gives %q the checksum type %q, and this reader verifies %s alone",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key, row.ChksumType, chksumTypeSHA256)
	}
	// Message, not verdict: a value that is not a digest yields the zero
	// array, which the comparison below would refuse anyway.
	want, ok := sha256FromHex(row.ChksumSha256)
	if !ok {
		return fmt.Errorf("%w: %s names %q as the digest of %q, which is not a sha256",
			helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, row.ChksumSha256, key)
	}
	got, err := resolveEntryDigest(scan, key)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: %q does not match the digest %s names for it",
			helpers.ErrManifestChainMismatch, key, helpers.FilesManifestFileName)
	}
	return nil
}

// resolveEntryDigest returns the digest of what key names, following link
// entries up to chainMaxLinkHops until it reaches a regular file.
func resolveEntryDigest(scan *archiveScan, key string) ([32]byte, error) {
	for hop := 0; hop <= chainMaxLinkHops; hop++ {
		if sum, ok := scan.hashes[key]; ok {
			return sum, nil
		}
		// Not a verdict of its own - an unresolvable name would hit the hop
		// bound - but this message names the actual defect.
		target, ok := scan.links[key]
		if !ok {
			return [32]byte{}, fmt.Errorf("%w: %s lists %q, which the archive does not carry",
				helpers.ErrManifestChainMismatch, helpers.FilesManifestFileName, key)
		}
		key = target
	}
	return [32]byte{}, fmt.Errorf("%w: %q resolves through more than %d links",
		helpers.ErrManifestChainMismatch, key, chainMaxLinkHops)
}

// checkUnlisted requires every retained entry to appear in FILES.json, since
// archive.ExtractTarGz writes what the tar carries, not what the listing names.
// MANIFEST.json and FILES.json are exempt: the signature chain covers both.
func checkUnlisted(scan *archiveScan, listed map[string]struct{}) error {
	for _, key := range scan.order {
		if key == helpers.ManifestFileName || key == helpers.FilesManifestFileName {
			continue
		}
		if _, ok := listed[key]; !ok {
			return fmt.Errorf("%w: the archive carries %q, which %s does not list",
				helpers.ErrManifestChainMismatch, key, helpers.FilesManifestFileName)
		}
	}
	return nil
}

// sha256FromHex decodes a digest helpers.IsSHA256Hex accepts into an array,
// reporting false for anything else. The nibble loop spares encoding/hex's
// per-row []byte conversion.
func sha256FromHex(s string) ([32]byte, bool) {
	var out [sha256.Size]byte
	if !helpers.IsSHA256Hex(s) {
		return out, false
	}
	for i := range out {
		high := hexNibble(s[i*hexDigitsPerByte])
		low := hexNibble(s[i*hexDigitsPerByte+1])
		out[i] = high<<hexHighNibbleShift | low
	}
	return out, true
}

// hexNibble is the value of one hex digit. It is total only for the alphabet
// helpers.IsSHA256Hex accepts, which every caller has already checked.
func hexNibble(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + hexLetterValue
}

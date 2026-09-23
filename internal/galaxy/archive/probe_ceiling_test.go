package archive

// This file measures how far archive/tar reads before Next returns its first
// header, the figure helpers.ArchiveProbeMaxBytes is derived from, by building
// the maximal composite that reaches it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"strconv"
	"strings"
	"testing"
)

const (
	// metaBodyMaxBytes is archive/tar's unexported maxSpecialFileSize: the 1 MiB
	// it allows one meta header's body and one sparse map alike.
	metaBodyMaxBytes = 1 << 20
	// sparseEntryName is the name of the composite's ordinary header, spelled
	// identically by its GNU.sparse.name record and its long-name meta headers.
	sparseEntryName = "collection/README.md"
	// compositeModTime stamps every block the composite builds, so the fixture
	// never depends on wall-clock time.
	compositeModTime = int64(1704067200) // 2024-01-01T00:00:00Z
)

// TestMetaHeaderCeilingIsWhatArchiveTarReads pins that archive/tar reads
// exactly 4,196,352 bytes of the maximal composite before its first header,
// refuses a map one block longer, and that ProbeTarGz accepts the composite.
func TestMetaHeaderCeilingIsWhatArchiveTarReads(t *testing.T) {
	t.Parallel()

	const (
		ceilingBytes     = 4_196_352
		acceptedMapBlock = 2048
		refusedMapBlocks = 2049
	)

	raw := buildMetaCeilingComposite(t, acceptedMapBlock)

	counter := &countingReader{r: bytes.NewReader(raw)}
	if _, err := tar.NewReader(counter).Next(); err != nil {
		t.Fatalf("maximal composite: Next = %v, want the ordinary header it ends on", err)
	}
	if counter.read != ceilingBytes {
		t.Fatalf("maximal composite: archive/tar read %d bytes before its first header, want %d",
			counter.read, ceilingBytes)
	}

	over := buildMetaCeilingComposite(t, refusedMapBlocks)
	if _, err := tar.NewReader(bytes.NewReader(over)).Next(); err == nil {
		t.Fatalf("composite with a %d-block sparse map: Next = %v, want a refusal",
			refusedMapBlocks, err)
	}

	var gzipped bytes.Buffer
	gz := gzip.NewWriter(&gzipped)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("failed to compress the maximal composite: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	if err := probeArchiveBytes(t, gzipped.Bytes()); err != nil {
		t.Fatalf("maximal composite: ProbeTarGz = %v, want it accepted inside the scan bound", err)
	}
}

// buildMetaCeilingComposite hand-assembles raw tar bytes, as tar.Writer refuses
// meta typeflags: maximal 'L', 'K' and 'x' bodies (one 'x' only, since a second
// would replace its sparse records), then a header over a mapBlocks-block map.
func buildMetaCeilingComposite(t *testing.T, mapBlocks int) []byte {
	t.Helper()

	sparseMap := buildPAXSparseMap(t, mapBlocks)

	// PAX 1.0 sparse records padded to a maximal body by one filler record. A
	// realsize of zero validates because every fragment in the map is empty.
	records := paxRecord("GNU.sparse.major", "1") +
		paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", sparseEntryName) +
		paxRecord("GNU.sparse.realsize", "0")
	records += paxFillerRecord(t, "comment", metaBodyMaxBytes-len(records))

	// The 'L' and 'K' bodies are a name followed by NUL padding rather than a
	// megabyte of text: what the ceiling counts is bytes read, which is the
	// declared body size either way.
	longName := make([]byte, metaBodyMaxBytes)
	copy(longName, sparseEntryName)

	var buf bytes.Buffer
	buf.Grow(3*(tarBlockSize+metaBodyMaxBytes) + tarBlockSize + len(sparseMap) + 2*tarBlockSize)
	for _, chunk := range [][]byte{
		gnuMetaHeaderBlock(tar.TypeGNULongName, metaBodyMaxBytes), longName,
		gnuMetaHeaderBlock(tar.TypeGNULongLink, metaBodyMaxBytes), longName,
		paxMetaHeaderBlock(metaBodyMaxBytes), []byte(records),
		ordinaryHeaderBlock(int64(len(sparseMap))), sparseMap,
		// The two zero blocks that terminate a tar stream. The walk stops on
		// the ordinary header above and never reads this far, but an archive
		// without them is not one archive/tar could have been handed.
		make([]byte, 2*tarBlockSize),
	} {
		buf.Write(chunk)
	}
	return buf.Bytes()
}

// buildPAXSparseMap renders a NUL-padded PAX 1.0 sparse map of exactly blocks
// tar blocks with the most "0\n0\n" pairs that fit; its last newline must land
// in the final block, or archive/tar would read one block fewer.
func buildPAXSparseMap(t *testing.T, blocks int) []byte {
	t.Helper()

	target := blocks * tarBlockSize
	mapLen := func(entries int) int { return len(strconv.Itoa(entries)) + 1 + 4*entries }

	// The largest fragment count that still fits: four bytes per fragment,
	// plus the count itself spelled once ahead of them and its own newline.
	entries := (target - 8) / 4
	for mapLen(entries+1) <= target {
		entries++
	}
	if entries < 1 || mapLen(entries) > target {
		t.Fatalf("no fragment count renders a sparse map of %d bytes", target)
	}
	if mapLen(entries) <= target-tarBlockSize {
		t.Fatalf("a %d-fragment map ends at %d bytes, short of the last of %d blocks",
			entries, mapLen(entries), blocks)
	}

	var buf bytes.Buffer
	buf.Grow(target)
	buf.WriteString(strconv.Itoa(entries))
	buf.WriteByte('\n')
	for range entries {
		buf.WriteString("0\n0\n")
	}
	buf.Write(make([]byte, target-buf.Len()))
	return buf.Bytes()
}

// paxRecord renders one "<len> <key>=<value>\n" PAX record. <len> counts its
// own digits, so it is computed twice: spelling it can cross a power of ten.
func paxRecord(key, value string) string {
	const padding = 3 // the space, the '=' and the newline

	size := len(key) + len(value) + padding
	size += len(strconv.Itoa(size))
	record := strconv.Itoa(size) + " " + key + "=" + value + "\n"
	if len(record) != size {
		record = strconv.Itoa(len(record)) + " " + key + "=" + value + "\n"
	}
	return record
}

// paxFillerRecord renders a record of exactly target bytes: archive/tar refuses
// an 'x' body holding a malformed record, so padding must be a record itself.
func paxFillerRecord(t *testing.T, key string, target int) string {
	t.Helper()

	const padding = 3 // the space, the '=' and the newline

	valueLen := target - len(key) - padding - len(strconv.Itoa(target))
	if valueLen < 1 {
		t.Fatalf("a %q record cannot be filled out to %d bytes", key, target)
	}
	record := paxRecord(key, strings.Repeat("a", valueLen))
	if len(record) != target {
		t.Fatalf("filler record is %d bytes, want %d", len(record), target)
	}
	return record
}

// gnuMetaHeaderBlock assembles one GNU meta header block ('L' or 'K') for a
// body of size bytes, carrying the GNU magic and version archive/tar needs to
// recognize the block as GNU format at all.
func gnuMetaHeaderBlock(typeflag byte, size int64) []byte {
	blk := newHeaderBlock("././@LongLink", typeflag, size)
	copy(blk[257:265], "ustar  \x00")
	sealTarBlock(blk)
	return blk
}

// paxMetaHeaderBlock assembles the 'x' block, spelling its name the way a real
// writer does.
func paxMetaHeaderBlock(size int64) []byte {
	blk := newHeaderBlock("PaxHeaders.0/README.md", tar.TypeXHeader, size)
	copy(blk[257:263], "ustar\x00")
	copy(blk[263:265], "00")
	sealTarBlock(blk)
	return blk
}

// ordinaryHeaderBlock assembles the header the walk returns. Its size field is
// the physical size, the sparse map after it; the logical size arrives in the
// 'x' header's GNU.sparse.realsize record.
func ordinaryHeaderBlock(size int64) []byte {
	blk := newHeaderBlock(sparseEntryName, tar.TypeReg, size)
	copy(blk[257:263], "ustar\x00")
	copy(blk[263:265], "00")
	sealTarBlock(blk)
	return blk
}

// newHeaderBlock fills the fields every block of this composite shares. Its
// caller supplies the magic and seals the checksum, since the magic is what
// separates the GNU blocks from the POSIX ones and the checksum covers it.
func newHeaderBlock(name string, typeflag byte, size int64) []byte {
	blk := make([]byte, tarBlockSize)
	copy(blk[0:100], name)           // name
	putTarOctal(blk[100:108], 0o644) // mode
	putTarOctal(blk[108:116], 0)     // uid
	putTarOctal(blk[116:124], 0)     // gid
	putTarOctal(blk[124:136], size)  // size
	putTarOctal(blk[136:148], compositeModTime)
	blk[156] = typeflag // typeflag
	return blk
}

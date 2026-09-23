package treearchive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Document is a lead entry written ahead of the tree, such as a manifest
// readers expect first: a regular file at the archive root with the same fixed
// mode and time as every other entry.
type Document struct {
	Name string
	Data []byte
}

// Write streams the lead documents, then the planned entries, into file and
// returns the lowercase hex sha256 of the compressed bytes. Every entry carries
// the commit time to the second; the caller owns file.
func (p *Plan) Write(ctx context.Context, file *os.File, lead []Document) (string, error) {
	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(file, h))
	tw := tar.NewWriter(gz)
	when := p.src.CommitTime().UTC().Truncate(time.Second)

	for _, doc := range lead {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: doc.Name, Size: int64(len(doc.Data)), Mode: modeFile, ModTime: when}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", fmt.Errorf("writing %s: %w", doc.Name, err)
		}
		if _, err := tw.Write(doc.Data); err != nil {
			return "", fmt.Errorf("writing %s: %w", doc.Name, err)
		}
	}
	for i := range p.entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := p.writeEntry(tw, &p.entries[i], when); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("closing the tar stream: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("closing the gzip stream: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeEntry writes one planned entry. A regular file is re-opened and must
// stream exactly the size the plan charged; a longer stream is refused
// before the tar writer sees a byte past it.
func (p *Plan) writeEntry(tw *tar.Writer, entry *plannedEntry, when time.Time) error {
	hdr := &tar.Header{
		Typeflag: entry.typeflag,
		Name:     entry.name,
		Linkname: entry.linkname,
		Size:     entry.size,
		Mode:     entry.mode,
		ModTime:  when,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing %s: %w", DisplayPath(entry.src), err)
	}
	if entry.typeflag != tar.TypeReg {
		return nil
	}
	r, err := p.src.Open(entry.src)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	n, err := io.CopyN(tw, r, entry.size)
	if err != nil || n != entry.size {
		return fmt.Errorf("%w: %s streamed %d bytes where the tree declares %d",
			helpers.ErrGitCommitMismatch, DisplayPath(entry.src), n, entry.size)
	}
	var probe [1]byte
	if extra, _ := r.Read(probe[:]); extra > 0 {
		return fmt.Errorf("%w: %s streams more than the %d bytes the tree declares",
			helpers.ErrGitCommitMismatch, DisplayPath(entry.src), entry.size)
	}
	return nil
}

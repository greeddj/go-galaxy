package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/klauspost/pgzip"
)

// ExtractTarGz extracts a tar.gz archive into dstDir with safety checks.
func ExtractTarGz(tarGzFile, dstDir string) error {
	info, err := os.Stat(tarGzFile)
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", tarGzFile, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%w: %s", helpers.ErrFileIsEmpty, tarGzFile)
	}

	//nolint:gosec // tarGzFile is a user-provided archive path expected by CLI.
	file, err := os.Open(tarGzFile)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	uncompressedStream, err := pgzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		_ = uncompressedStream.Close()
	}()

	tarReader := tar.NewReader(uncompressedStream)
	return extractTarEntries(tarReader, dstDir)
}

func extractTarEntries(tarReader *tar.Reader, dstDir string) error {
	var extracted int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("error reading tar archive: %w", err)
		}
		if err := handleTarEntry(tarReader, header, dstDir, &extracted); err != nil {
			return err
		}
	}
}

func handleTarEntry(tarReader *tar.Reader, header *tar.Header, dstDir string, extracted *int64) error {
	relPath, err := sanitizeArchivePath(header.Name)
	if err != nil {
		return err
	}
	if relPath == "" {
		return nil
	}
	targetPath := filepath.Join(dstDir, relPath)
	if err := ensureNoSymlinkParents(dstDir, relPath); err != nil {
		return err
	}

	switch header.Typeflag {
	case tar.TypeDir:
		return extractDir(targetPath)
	case tar.TypeReg:
		return extractRegularFile(tarReader, header, targetPath, extracted)
	case tar.TypeSymlink:
		return extractSymlink(relPath, targetPath, header)
	case tar.TypeLink:
		return extractHardlink(dstDir, targetPath, header)
	default:
		return nil
	}
}

func extractDir(targetPath string) error {
	if err := os.MkdirAll(targetPath, helpers.DirMod); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetPath, err)
	}
	return nil
}

func extractRegularFile(tarReader *tar.Reader, header *tar.Header, targetPath string, extracted *int64) error {
	if header.Size < 0 {
		return fmt.Errorf("%w: %s ", helpers.ErrArchiveEntryHasNegativeSize, header.Name)
	}
	if header.Size > helpers.ArchiveMaxEntrySize {
		return fmt.Errorf("%w %s: %d bytes", helpers.ErrArchiveEntryIsTooLarge, header.Name, header.Size)
	}
	if *extracted+header.Size > helpers.ArchiveMaxTotalSize {
		return fmt.Errorf("%w: %d bytes", helpers.ErrArchiveExceedsMaxSize, helpers.ArchiveMaxTotalSize)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), helpers.DirMod); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	mode := header.FileInfo().Mode().Perm()
	//nolint:gosec // targetPath is sanitized archive entry under dstDir.
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", targetPath, err)
	}
	written, err := io.CopyN(file, tarReader, header.Size)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("failed to write file %s: %w", targetPath, err)
	}
	*extracted += written
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close file %s: %w", targetPath, err)
	}
	return nil
}

func extractSymlink(relPath, targetPath string, header *tar.Header) error {
	linkTarget, err := safeSymlinkTarget(relPath, header.Linkname)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), helpers.DirMod); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	if err := os.Symlink(linkTarget, targetPath); err != nil {
		return fmt.Errorf("failed to create symlink %s -> %s: %w", targetPath, linkTarget, err)
	}
	return nil
}

func extractHardlink(dstDir, targetPath string, header *tar.Header) error {
	linkRel, err := sanitizeArchivePath(header.Linkname)
	if err != nil {
		return err
	}
	if linkRel == "" {
		return fmt.Errorf("%w for %s", helpers.ErrHardlinkTargetIsEmpty, header.Name)
	}
	if err := ensureNoSymlinkParents(dstDir, linkRel); err != nil {
		return err
	}
	target := filepath.Join(dstDir, linkRel)
	if err := os.MkdirAll(filepath.Dir(targetPath), helpers.DirMod); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", targetPath, err)
	}
	if err := os.Link(target, targetPath); err != nil {
		return fmt.Errorf("failed to create hardlink %s -> %s: %w", targetPath, target, err)
	}
	return nil
}

// FileHashSHA256 calculates the SHA256 hash of a file on disk.
func FileHashSHA256(path string) (string, error) {
	//nolint:gosec // path is caller-provided and expected for hashing.
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = f.Close()
	}()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeArchivePath validates and normalizes a tar entry path.
func sanitizeArchivePath(name string) (string, error) {
	if name == "" {
		return "", helpers.ErrArchiveEntryHasEmptyName
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." {
		return "", nil
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: %s", helpers.ErrArchiveEntryIsAbsolutePath, name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", helpers.ErrArchiveEntryEscapesDestination, name)
	}
	return cleaned, nil
}

// ensureNoSymlinkParents rejects paths that traverse symlink parents.
func ensureNoSymlinkParents(baseDir, relPath string) error {
	if relPath == "" || relPath == "." {
		return nil
	}
	current := baseDir
	for part := range strings.SplitSeq(relPath, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("failed to stat path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", helpers.ErrArchivePathContainsSymlinkComponent, current)
		}
	}
	return nil
}

// safeSymlinkTarget validates a symlink target within the archive.
func safeSymlinkTarget(relPath, linkName string) (string, error) {
	if linkName == "" {
		return "", fmt.Errorf("%w for %s", helpers.ErrSymlinkTargetIsEmpty, relPath)
	}
	if filepath.IsAbs(linkName) || filepath.VolumeName(linkName) != "" {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetIsAbsolute, linkName)
	}
	cleaned := filepath.Clean(filepath.FromSlash(linkName))
	if cleaned == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTarget, linkName)
	}
	baseDir := filepath.Dir(relPath)
	resolved := filepath.Clean(filepath.Join(baseDir, cleaned))
	if resolved == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetResolvesToRoot, linkName)
	}
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetEscapesDestination, linkName)
	}
	relTarget, err := filepath.Rel(baseDir, resolved)
	if err != nil {
		return "", err
	}
	if relTarget == "." {
		return "", fmt.Errorf("%w: %s", helpers.ErrSymlinkTargetResolvesToSelf, linkName)
	}
	return relTarget, nil
}

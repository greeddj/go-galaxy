package collectionbuild

import (
	"fmt"
	"io"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

// Discover finds the collection directories under subdir by ansible's dir
// classification, one level deep. Only a regular galaxy.yml or MANIFEST.json
// marks a directory; two candidates naming one namespace.name are refused.
func Discover(src Source, subdir string) ([]Candidate, []string, error) {
	base, entries, err := resolveSubdir(src, subdir)
	if err != nil {
		return nil, nil, err
	}
	dirs, err := candidateDirs(src, base, entries)
	if err != nil {
		return nil, nil, err
	}
	if len(dirs) == 0 {
		return nil, nil, fmt.Errorf("%w: %s carries no %s or %s, and neither does any of its immediate subdirectories",
			helpers.ErrGitCollectionNotFound, treearchive.DisplayPath(base), galaxyYMLName, helpers.ManifestFileName)
	}

	candidates := make([]Candidate, 0, len(dirs))
	var warnings []string
	seen := make(map[string]string, len(dirs))
	for _, dir := range dirs {
		cand, warns, err := readCandidate(src, dir)
		if err != nil {
			return nil, nil, err
		}
		fqcn := cand.Meta.Namespace + "." + cand.Meta.Name
		if first, dup := seen[fqcn]; dup {
			return nil, nil, fmt.Errorf("%w: %s is declared by both %s and %s",
				helpers.ErrGitDuplicateCollection, fqcn, treearchive.DisplayPath(first), treearchive.DisplayPath(dir))
		}
		seen[fqcn] = dir
		for _, w := range warns {
			warnings = append(warnings, treearchive.DisplayPath(dir)+": "+w)
		}
		candidates = append(candidates, cand)
	}
	return candidates, warnings, nil
}

// candidateDirs applies the classification: base itself when it lists
// metadata, otherwise its qualifying immediate children.
func candidateDirs(src Source, base string, entries []Entry) ([]string, error) {
	kind, err := classify(entries, base)
	if err != nil {
		return nil, err
	}
	if kind != metaNone {
		return []string{base}, nil
	}
	var dirs []string
	for _, e := range entries {
		if e.Kind != EntryDir || strings.HasPrefix(e.Name, ".") {
			continue
		}
		child := treearchive.JoinPath(base, e.Name)
		childEntries, err := src.ReadDir(child)
		if err != nil {
			return nil, err
		}
		childKind, err := classify(childEntries, child)
		if err != nil {
			return nil, err
		}
		if childKind != metaNone {
			dirs = append(dirs, child)
		}
	}
	return dirs, nil
}

// metaKind says which metadata file a directory lists.
type metaKind uint8

const (
	metaNone metaKind = iota
	metaGalaxyYML
	metaManifest
)

// classify looks for the two metadata files among entries. Only a regular
// file counts; a directory or a link under either name is not metadata.
func classify(entries []Entry, dir string) (metaKind, error) {
	hasGalaxy := listsFile(entries, galaxyYMLName)
	hasManifest := listsFile(entries, helpers.ManifestFileName)
	switch {
	case hasGalaxy && hasManifest:
		return metaNone, fmt.Errorf("%w: %s has both a %s and a %s",
			helpers.ErrGalaxyYMLInvalid, treearchive.DisplayPath(dir), helpers.ManifestFileName, galaxyYMLName)
	case hasManifest:
		return metaManifest, nil
	case hasGalaxy:
		return metaGalaxyYML, nil
	default:
		return metaNone, nil
	}
}

func listsFile(entries []Entry, name string) bool {
	for _, e := range entries {
		if e.Name == name && (e.Kind == EntryFile || e.Kind == EntryExecutable) {
			return true
		}
	}
	return false
}

// resolveSubdir walks subdir component by component and returns the clean
// path with the listing of the directory it names.
func resolveSubdir(src Source, subdir string) (string, []Entry, error) {
	entries, err := src.ReadDir("")
	if err != nil {
		return "", nil, err
	}
	if subdir == "" {
		return "", entries, nil
	}
	cur := ""
	for component := range strings.SplitSeq(subdir, "/") {
		if !helpers.IsPathElement(component) {
			return "", nil, fmt.Errorf("%w: subdir %q is not a relative path of plain components",
				helpers.ErrGitCollectionNotFound, helpers.TruncateForMessage(string(safeout.Clean(subdir))))
		}
		found := false
		for _, e := range entries {
			if e.Name != component {
				continue
			}
			if e.Kind != EntryDir {
				return "", nil, fmt.Errorf("%w: %s is not a directory",
					helpers.ErrGitCollectionNotFound, treearchive.DisplayPath(treearchive.JoinPath(cur, component)))
			}
			found = true
			break
		}
		if !found {
			return "", nil, fmt.Errorf("%w: %s does not exist",
				helpers.ErrGitCollectionNotFound, treearchive.DisplayPath(treearchive.JoinPath(cur, component)))
		}
		cur = treearchive.JoinPath(cur, component)
		entries, err = src.ReadDir(cur)
		if err != nil {
			return "", nil, err
		}
	}
	return cur, entries, nil
}

// readCandidate reads the metadata of the collection directory at dir,
// preferring MANIFEST.json as ansible's _get_meta_from_dir does; classify
// has already refused the directory carrying both.
func readCandidate(src Source, dir string) (Candidate, []string, error) {
	entries, err := src.ReadDir(dir)
	if err != nil {
		return Candidate{}, nil, err
	}
	kind, err := classify(entries, dir)
	if err != nil {
		return Candidate{}, nil, err
	}
	name := galaxyYMLName
	if kind == metaManifest {
		name = helpers.ManifestFileName
	}
	data, err := readMetadataFile(src, treearchive.JoinPath(dir, name))
	if err != nil {
		return Candidate{}, nil, err
	}
	if kind == metaManifest {
		meta, err := parseManifestInfo(data)
		if err != nil {
			return Candidate{}, nil, fmt.Errorf("%s: %w", treearchive.DisplayPath(dir), err)
		}
		return Candidate{Meta: meta, Subdir: dir, FromManifest: true}, nil, nil
	}
	meta, warnings, err := ParseGalaxyYML(data)
	if err != nil {
		return Candidate{}, nil, fmt.Errorf("%s: %w", treearchive.DisplayPath(dir), err)
	}
	return Candidate{Meta: meta, Subdir: dir}, warnings, nil
}

// readMetadataFile reads one metadata file under the metadata cap. The
// reader stops one byte past the cap so the refusal is decided here rather
// than by the Source's larger per-entry cap.
func readMetadataFile(src Source, p string) ([]byte, error) {
	r, err := src.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, metadataMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", treearchive.DisplayPath(p), err)
	}
	if len(data) > metadataMaxBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", helpers.ErrGalaxyYMLInvalid, treearchive.DisplayPath(p), metadataMaxBytes)
	}
	return data, nil
}

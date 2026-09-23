package rolebuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
)

// gitDirName is the one directory name the build excludes at every depth.
const gitDirName = ".git"

// installInfoPattern is ansible-galaxy's install record, left out of the
// artifact: the install writes a fresh one, and a committed one would be a
// read-only hard link that write has to replace.
const installInfoPattern = "meta/.galaxy_install_info"

// Build turns the role at the root of src into a tar.gz in the file tempFile
// supplies, reading the metadata first so a non-role is refused before the
// walk. The result's Cleanup is idempotent; on any error the file is gone.
func Build(ctx context.Context, src treearchive.Source, tempFile TempFileFunc) (Built, error) {
	if tempFile == nil {
		return Built{}, fmt.Errorf("%w: no temp file supplier", helpers.ErrConfigIsNil)
	}
	meta, err := readMeta(src)
	if err != nil {
		return Built{}, err
	}
	plan, err := treearchive.PlanTree(ctx, src, treearchive.Options{
		Subject: "the role",
		Rules:   treearchive.Rules{DirNames: map[string]struct{}{gitDirName: {}}, Patterns: []string{installInfoPattern}},
	})
	if err != nil {
		return Built{}, err
	}

	file, cleanup, err := tempFile(ctx)
	if err != nil {
		return Built{}, err
	}
	artifactPath := file.Name()
	digest, err := plan.Write(ctx, file, nil)
	if err != nil {
		_ = file.Close()
		cleanup()
		return Built{}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return Built{}, fmt.Errorf("closing %s: %w", artifactPath, err)
	}
	if err := selfCheck(ctx, artifactPath); err != nil {
		cleanup()
		return Built{}, err
	}

	var once sync.Once
	return Built{
		Cleanup:      func() { once.Do(cleanup) },
		Meta:         meta,
		ArtifactPath: artifactPath,
		SHA256:       digest,
		Warnings:     append(slices.Clone(plan.Warnings()), meta.Warnings...),
	}, nil
}

// selfCheck probes the artifact the way the extractor will. A failure is a
// builder defect, rendered with %v under helpers.ErrGitArtifactSelfCheck so it
// never classifies as the remote's integrity failure; cancellation passes as is.
func selfCheck(ctx context.Context, artifactPath string) error {
	err := archive.ProbeTarGz(ctx, artifactPath)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %v", helpers.ErrGitArtifactSelfCheck, err)
	}
}

// readMeta reads meta/main.yml (else .yaml) for dependencies and role name,
// then appends meta/requirements.yml's list. No meta directory, or no main
// file there as a regular file, makes the tree not a role.
func readMeta(src treearchive.Source) (Meta, error) {
	root, err := src.ReadDir("")
	if err != nil {
		return Meta{}, err
	}
	if !listsKind(root, metaDirName, treearchive.EntryDir) {
		return Meta{}, fmt.Errorf("%w: the repository root has no %s directory", helpers.ErrRoleMetaNotFound, metaDirName)
	}
	entries, err := src.ReadDir(metaDirName)
	if err != nil {
		return Meta{}, err
	}
	meta, err := readMain(src, entries)
	if err != nil {
		return Meta{}, err
	}
	deps, warnings, err := readRequirements(src, entries)
	if err != nil {
		return Meta{}, err
	}
	meta.Dependencies = append(meta.Dependencies, deps...)
	meta.Warnings = append(meta.Warnings, warnings...)
	return meta, nil
}

// readMain reads whichever main spelling the meta directory lists.
func readMain(src treearchive.Source, entries []treearchive.Entry) (Meta, error) {
	name, err := pickOne(entries, mainYMLName, mainYAMLName)
	if err != nil {
		return Meta{}, err
	}
	if name == "" {
		return Meta{}, fmt.Errorf("%w: %s carries neither %s nor %s", helpers.ErrRoleMetaNotFound, metaDirName, mainYMLName, mainYAMLName)
	}
	p := treearchive.JoinPath(metaDirName, name)
	data, err := readMetadataFile(src, p)
	if err != nil {
		return Meta{}, err
	}
	return parseMetaMain(data, p)
}

// readRequirements reads whichever requirements spelling the meta directory
// lists; neither is an empty list.
func readRequirements(src treearchive.Source, entries []treearchive.Entry) ([]gitsource.RoleDependency, []string, error) {
	name, err := pickOne(entries, requirementsYMLName, requirementsYAMLName)
	if err != nil || name == "" {
		return nil, nil, err
	}
	p := treearchive.JoinPath(metaDirName, name)
	data, err := readMetadataFile(src, p)
	if err != nil {
		return nil, nil, err
	}
	return parseMetaRequirements(data, p)
}

// pickOne returns whichever spelling entries list as a regular file, "" for
// neither, and a refusal for both: ansible would read the first, and two
// spellings that disagree have no one answer.
func pickOne(entries []treearchive.Entry, yml, yaml string) (string, error) {
	hasYML := listsFile(entries, yml)
	hasYAML := listsFile(entries, yaml)
	switch {
	case hasYML && hasYAML:
		return "", fmt.Errorf("%w: %s carries both %s and %s", helpers.ErrRoleMetaInvalid, metaDirName, yml, yaml)
	case hasYML:
		return yml, nil
	case hasYAML:
		return yaml, nil
	default:
		return "", nil
	}
}

// listsFile reports whether entries carry name as a regular file. A link
// under the name is not metadata: following it would mean reading a blob the
// walk's own symlink rules may later skip.
func listsFile(entries []treearchive.Entry, name string) bool {
	return listsKind(entries, name, treearchive.EntryFile) || listsKind(entries, name, treearchive.EntryExecutable)
}

func listsKind(entries []treearchive.Entry, name string, kind treearchive.EntryKind) bool {
	for _, e := range entries {
		if e.Name == name && e.Kind == kind {
			return true
		}
	}
	return false
}

// readMetadataFile reads one metadata file under the metadata cap. The
// reader stops one byte past the cap so the refusal is decided here rather
// than by the Source's larger per-entry cap.
func readMetadataFile(src treearchive.Source, p string) ([]byte, error) {
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
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", helpers.ErrRoleMetaInvalid, treearchive.DisplayPath(p), metadataMaxBytes)
	}
	return data, nil
}

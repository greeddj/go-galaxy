package collections

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// extractCollection materializes a collection tarball into target.path, via
// the extracted store when one is given. Only the reset and the marker go
// through target.root; unpack gets a plain path the reset just recreated.
func extractCollection(
	ctx context.Context,
	col collection,
	tarPath string,
	target installTarget,
	runtime *infra.Infra,
	extractStore *extracted.Store,
	artifactSHA string,
	artifactSHAComputed bool,
) error {
	return extractTree(ctx, col.Namespace+"/"+col.Name, tarPath, target, runtime, extractStore, artifactSHA, artifactSHAComputed, nil)
}

// extractTree resets, unpacks and marks a collection or role tree.
// postExtract runs before the marker is written, so what it adds (a role's
// meta/.galaxy_install_info) is in the tally rather than read as drift.
func extractTree(
	ctx context.Context,
	display string,
	tarPath string,
	target installTarget,
	runtime *infra.Infra,
	extractStore *extracted.Store,
	artifactSHA string,
	artifactSHAComputed bool,
	postExtract func(installTarget) error,
) error {
	if artifactSHA == "" {
		hash, err := archive.FileHashSHA256(tarPath)
		if err != nil {
			return err
		}
		artifactSHA = hash
		artifactSHAComputed = true
	}
	// Refused before the destructive reset, not left to writeExtractMarker's
	// own guard, which would fire only after target.path was wiped and
	// re-extracted for a value that was never usable.
	if !helpers.IsSHA256Hex(artifactSHA) {
		return fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, artifactSHA)
	}
	if verifyExtractMarker(runtime.Output, target, artifactSHA) {
		runtime.Output.Printf("Skipping extraction, already done: %s", display)
		return nil
	}

	if err := resetExtractionTarget(target); err != nil {
		return err
	}

	if err := unpack(ctx, tarPath, target.path, extractStore, artifactSHA, artifactSHAComputed); err != nil {
		return err
	}
	if postExtract != nil {
		if err := postExtract(target); err != nil {
			return err
		}
	}

	return writeExtractMarker(target, artifactSHA)
}

// resetExtractionTarget empties everything a real extraction is about to
// rewrite: the tree, and for a collection its .info directories as well (see
// resetCollectionInfo), before anything is unpacked.
func resetExtractionTarget(target installTarget) error {
	// A failure here may mean the previous tree is gone, so it is classified,
	// never discarded; a symlinked ancestor makes the rooted remove refuse
	// before anything is destroyed.
	if err := target.root.RemoveAll(target.rel); err != nil {
		return classifyCollectionsRootError(target.root, target.rel, err)
	}
	if err := target.root.MkdirAll(target.rel, helpers.DirMod); err != nil {
		return classifyCollectionsRootError(target.root, target.rel, err)
	}
	if target.infoPrefix == "" {
		return nil
	}
	return resetCollectionInfo(target)
}

// resetCollectionInfo removes every "<namespace>.<name>-<version>.info" of
// target's collection and recreates its own version's, as ansible-galaxy does,
// so no stale marker outlives the tree it counted.
func resetCollectionInfo(target installTarget) error {
	entries, err := fs.ReadDir(target.root.FS(), collectionsDirName)
	if err != nil {
		return classifyCollectionsRootError(target.root, collectionsDirName, err)
	}
	for _, entry := range entries {
		version, ok := strings.CutPrefix(entry.Name(), target.infoPrefix)
		if !ok {
			continue
		}
		version, ok = strings.CutSuffix(version, infoDirSuffix)
		if !ok || !helpers.IsExactVersion(version) {
			continue
		}
		rel := path.Join(collectionsDirName, entry.Name())
		if err := target.root.RemoveAll(rel); err != nil {
			return classifyCollectionsRootError(target.root, rel, err)
		}
	}
	if err := target.root.MkdirAll(target.info, helpers.DirMod); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	return nil
}

func unpack(
	ctx context.Context,
	tarPath, installPath string,
	extractStore *extracted.Store,
	artifactSHA string,
	artifactSHAComputed bool,
) error {
	if extractStore == nil {
		return archive.ExtractTarGz(ctx, tarPath, installPath)
	}
	src, err := extractStore.Ensure(ctx, artifactSHA, tarPath, shaProvenance(artifactSHAComputed))
	if err != nil {
		return err
	}
	return extracted.Materialize(src, installPath)
}

// shaProvenance maps "this process hashed these bytes" onto the store's
// provenance: any sha not self-computed is re-verified against the file's
// bytes before it keys the shared CAS.
func shaProvenance(selfComputed bool) extracted.SHAProvenance {
	if selfComputed {
		return extracted.SHASelfComputed
	}
	return extracted.SHAFromRecord
}

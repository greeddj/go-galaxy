package collectionbuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
)

// documentsPerArchive is the two entries every artifact carries ahead of
// the tree: MANIFEST.json and FILES.json. They count against the entry
// budget like any other, which is why the plan reserves them.
const documentsPerArchive = 2

// Build writes one candidate as a tar.gz into the file tempFile supplies,
// planning and hashing the tree first so MANIFEST.json and FILES.json lead the
// archive. Built.Cleanup is idempotent; on any error the file is already gone.
func Build(ctx context.Context, src Source, cand Candidate, tempFile TempFileFunc) (Built, error) {
	if tempFile == nil {
		return Built{}, fmt.Errorf("%w: no temp file supplier", helpers.ErrConfigIsNil)
	}
	plan, err := treearchive.PlanTree(ctx, src, treearchive.Options{
		Root:     cand.Subdir,
		Rules:    newIgnoreRules(cand.Meta.Namespace, cand.Meta.Name, cand.Meta.BuildIgnore),
		Reserved: documentsPerArchive,
		Digests:  true,
		Subject:  "the collection",
	})
	if err != nil {
		return Built{}, err
	}
	manifestJSON, filesJSON, err := encodeDocuments(&cand.Meta, listingRows(plan.Rows()))
	if err != nil {
		return Built{}, fmt.Errorf("encoding %s: %w", helpers.ManifestFileName, err)
	}
	// Measured before writing so a listing past FilesManifestMaxBytes fails as
	// the budget it broke rather than as a self-check defect of the builder.
	if int64(len(filesJSON)) > helpers.FilesManifestMaxBytes {
		return Built{}, fmt.Errorf("%w: %s is %d bytes, the limit is %d",
			helpers.ErrArchiveEntryIsTooLarge, helpers.FilesManifestFileName, len(filesJSON), helpers.FilesManifestMaxBytes)
	}

	file, cleanup, err := tempFile(ctx)
	if err != nil {
		return Built{}, err
	}
	artifactPath := file.Name()
	lead := []treearchive.Document{
		{Name: helpers.ManifestFileName, Data: manifestJSON},
		{Name: helpers.FilesManifestFileName, Data: filesJSON},
	}
	digest, err := plan.Write(ctx, file, lead)
	if err != nil {
		_ = file.Close()
		cleanup()
		return Built{}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return Built{}, fmt.Errorf("closing %s: %w", artifactPath, err)
	}
	if err := selfCheck(ctx, artifactPath, manifestJSON); err != nil {
		cleanup()
		return Built{}, err
	}

	var once sync.Once
	deps := make(map[string]string, len(cand.Meta.Dependencies))
	maps.Copy(deps, cand.Meta.Dependencies)
	return Built{
		Cleanup:      func() { once.Do(cleanup) },
		Dependencies: deps,
		Namespace:    cand.Meta.Namespace,
		Name:         cand.Meta.Name,
		Version:      cand.Meta.Version,
		Subdir:       cand.Subdir,
		ArtifactPath: artifactPath,
		SHA256:       digest,
		Warnings:     plan.Warnings(),
	}, nil
}

// listingRows renders the plan's rows as FILES.json rows behind the root
// row, in the plan's order.
func listingRows(rows []treearchive.Row) []filesRow {
	out := make([]filesRow, 0, len(rows)+1)
	out = append(out, dirRow(rootRowName))
	for _, r := range rows {
		if r.Dir {
			out = append(out, dirRow(r.Name))
		} else {
			out = append(out, fileRow(r.Name, r.Digest))
		}
	}
	return out
}

// selfCheck reads the artifact back as the pipeline will and verifies the
// chain. A failure is this package's defect, so the cause is %v, never %w:
// it must not classify as an integrity failure of the remote's bytes.
func selfCheck(ctx context.Context, artifactPath string, manifestJSON []byte) error {
	readBack, err := manifest.ReadFromTarGz(ctx, artifactPath)
	if err != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: reading the manifest back: %v", helpers.ErrGitArtifactSelfCheck, err)
	}
	if !bytes.Equal(readBack, manifestJSON) {
		return fmt.Errorf("%w: the manifest read back differs from the one written", helpers.ErrGitArtifactSelfCheck)
	}
	if err := manifest.VerifyChain(ctx, artifactPath, manifestJSON); err != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %v", helpers.ErrGitArtifactSelfCheck, err)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

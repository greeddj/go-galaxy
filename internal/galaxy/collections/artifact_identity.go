package collections

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
)

// checkArtifactIdentity reads the downloaded artifact's MANIFEST.json and
// refuses, as mismatch, an identity other than the collection being installed.
func checkArtifactIdentity(ctx context.Context, col collection, path, display string, mismatch error) error {
	manifestBytes, err := manifest.ReadFromTarGz(ctx, path)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	meta, err := collectionbuild.ParseManifestInfo(manifestBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", display, err)
	}
	if meta.Namespace != col.Namespace || meta.Name != col.Name || meta.Version != col.Version {
		return fmt.Errorf("%w: %s serves %s.%s@%s for %s",
			mismatch, display, meta.Namespace, meta.Name, meta.Version, col.key())
	}
	return nil
}

// checkLockedArtifactIdentity holds bytes a lockfile's download_url served to
// the entry before they fill its cache slot: that URL's path is free, so the
// manifest, not the path, says which collection the bytes are.
func checkLockedArtifactIdentity(ctx context.Context, col collection, path string) error {
	if col.DownloadURL == "" {
		return nil
	}
	return checkArtifactIdentity(ctx, col, path, helpers.URLForMessage(col.DownloadURL), helpers.ErrLockedArtifactIdentityMismatch)
}

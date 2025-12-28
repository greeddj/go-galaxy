package collections

import (
	"os"
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// extractCollection unpacks a collection tarball into the install path.
func extractCollection(col collection, tarPath, installPath string, runtime *infra.Infra, artifactSHA string) error {
	if artifactSHA == "" {
		hash, err := archive.FileHashSHA256(tarPath)
		if err != nil {
			return err
		}
		artifactSHA = hash
	}
	cacheTag := filepath.Join(installPath, ".extract-done."+artifactSHA)

	if _, err := os.Stat(cacheTag); err == nil {
		runtime.Output.Printf("⏭️ Skipping extraction, already done: %s/%s", col.Namespace, col.Name)
		return nil
	}

	_ = os.RemoveAll(installPath)
	if err := os.MkdirAll(installPath, dirMod); err != nil {
		return err
	}

	if err := archive.ExtractTarGz(tarPath, installPath); err != nil {
		return err
	}

	if err := os.WriteFile(cacheTag, []byte("ok"), fileMod); err != nil {
		return err
	}

	return nil
}

package collections

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/psvmcc/hub/pkg/types"
	"gopkg.in/yaml.v3"
)

const (
	dirMod  = 0o755
	fileMod = 0o644
)

// GalaxyYAML represents the GALAXY.yml metadata file.
type GalaxyYAML struct {
	DownloadURL string `yaml:"download_url"`
	FormatVer   string `yaml:"format_version"`
	Name        string `yaml:"name"`
	Namespace   string `yaml:"namespace"`
	Server      string `yaml:"server"`
	Signatures  any    `yaml:"signatures"`
	Version     string `yaml:"version"`
	VersionURL  string `yaml:"version_url"`
}

// writeGalaxyInfo writes GALAXY.yml for the installed collection.
func writeGalaxyInfo(cfg *config.Config, meta *types.GalaxyCollectionVersionInfo) error {
	if meta == nil {
		return nil
	}
	infoDir := filepath.Join(
		cfg.DownloadPath,
		"ansible_collections",
		fmt.Sprintf("%s.%s-%s.info", meta.Namespace.Name, meta.Name, meta.Version),
	)
	if err := os.MkdirAll(infoDir, dirMod); err != nil {
		return err
	}

	g := GalaxyYAML{
		DownloadURL: meta.DownloadURL,
		FormatVer:   "1.0.0",
		Name:        meta.Name,
		Namespace:   meta.Namespace.Name,
		Server:      cfg.Server,
		Signatures:  meta.Signatures,
		Version:     meta.Version,
		VersionURL:  meta.Href,
	}

	data, err := yaml.Marshal(&g)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), data, fileMod)
}

package collections

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// infoDirSuffix ends the `<namespace>.<name>-<version>.info` sidecar directory
// name. The layout is ansible's, so a tree ansible-galaxy installed scans the
// same as one this tool installed.
const infoDirSuffix = ".info"

// installedScan is what one walk of a collections tree found: the entries
// outdated can look up, and the installs it deliberately left out because
// nothing on disk says what to ask about them.
type installedScan struct {
	entries []lockfile.Entry
	// skippedGit names every git-sourced install, so the run can say which
	// collections its report does not cover rather than count them current.
	skippedGit []string
}

// scanInstalledTree builds outdated's current side from the tree at
// cfg.DownloadPath, with no cache backend. It walks the sidecars, the only
// record of each server, and trusts one only once the tree confirms it.
func scanInstalledTree(cfg *config.Config, runtime *infra.Infra) (installedScan, error) {
	root, err := os.OpenRoot(cfg.DownloadPath)
	if err != nil {
		return installedScan{}, err
	}
	defer func() { _ = root.Close() }()

	infos, err := fs.ReadDir(root.FS(), collectionsDirName)
	if err != nil {
		return installedScan{}, err
	}

	var scan installedScan
	for _, info := range infos {
		if !info.IsDir() || !strings.HasSuffix(info.Name(), infoDirSuffix) {
			continue
		}
		entry, kind := scanInstalledCollection(root, cfg, runtime, info.Name())
		switch kind {
		case installedGalaxy, installedURL:
			scan.entries = append(scan.entries, entry)
		case installedGit:
			scan.skippedGit = append(scan.skippedGit, entry.Name)
		case installedUnusable:
		}
	}
	// Sorted so the skipped-git warning, which nothing sorts downstream, reads
	// the same across two runs over an unchanged tree.
	slices.Sort(scan.skippedGit)
	slices.SortFunc(scan.entries, func(a, b lockfile.Entry) int { return strings.Compare(a.Name, b.Name) })
	return scan, nil
}

// installedKind classifies what one sidecar describes, which decides whether
// its collection can be looked up at all.
type installedKind int

const (
	// installedUnusable is a sidecar this scan will not build an entry from:
	// missing, unreadable, self-inconsistent, or naming a version the
	// installed tree does not hold.
	installedUnusable installedKind = iota
	installedGalaxy
	installedURL
	installedGit
)

// scanInstalledCollection turns one sidecar into a lockfile entry, trusted only
// when its fields recompose the directory name and MANIFEST.json holds its
// version. A git install records no ref, so it is returned as installedGit.
func scanInstalledCollection(
	root *os.Root, cfg *config.Config, runtime *infra.Infra, infoName string,
) (lockfile.Entry, installedKind) {
	rel := path.Join(collectionsDirName, infoName, galaxyYAMLFileName)
	doc, prov, ok := readInstalledSidecar(root, cfg, runtime, infoName)
	if !ok {
		return lockfile.Entry{}, installedUnusable
	}
	kind := installedKindOf(prov)
	name := doc.Namespace + "." + doc.Name
	if !installedNameUsable(kind, doc) || !helpers.IsExactVersion(doc.Version) {
		runtime.Output.Warnf("Skipping sidecar %s: it names no collection this tool can look up", displayPath(cfg, rel))
		return lockfile.Entry{}, installedUnusable
	}
	if infoName != fmt.Sprintf("%s-%s%s", name, doc.Version, infoDirSuffix) {
		runtime.Output.Warnf("Skipping sidecar %s: it describes %s@%s, not the collection it is filed under",
			displayPath(cfg, rel), name, doc.Version)
		return lockfile.Entry{}, installedUnusable
	}
	if !installedVersionMatches(root, doc) {
		return lockfile.Entry{}, installedUnusable
	}
	entry := lockfile.Entry{Name: name, Version: doc.Version, Source: doc.Server}
	switch kind {
	case installedGit:
		return entry, installedGit
	case installedURL:
		entry.Type = lockfile.TypeURL
		return entry, installedURL
	case installedGalaxy, installedUnusable:
	}
	if entry.Source == "" {
		entry.Source = cfg.Server
	}
	return entry, installedGalaxy
}

// installedKindOf classifies a sidecar by the provenance field only its source
// writes: a git commit, a url sha256, or neither for a Galaxy install.
func installedKindOf(prov sidecarProvenance) installedKind {
	switch {
	case prov.GitCommit != "":
		return installedGit
	case prov.URLSHA256 != "":
		return installedURL
	default:
		return installedGalaxy
	}
}

// installedNameUsable holds a sidecar's name to its source's alphabet: the
// Galaxy one, or ansible's wider FQCN rule for a url install, so the report
// drops no collection install accepted; see helpers.IsURLCollectionNamePart.
func installedNameUsable(kind installedKind, doc GalaxyYAML) bool {
	if kind == installedURL {
		return helpers.IsURLCollectionNamePart(doc.Namespace) && helpers.IsURLCollectionNamePart(doc.Name)
	}
	return helpers.IsCollectionNamePart(doc.Namespace) && helpers.IsCollectionNamePart(doc.Name)
}

// installedVersionMatches reports whether the tree's MANIFEST.json declares
// doc's version. A mismatch is a stale sidecar an earlier upgrade left; it and
// a missing or unreadable manifest contribute nothing and draw no warning.
func installedVersionMatches(root *os.Root, doc GalaxyYAML) bool {
	rel := path.Join(collectionsDirName, doc.Namespace, doc.Name, helpers.ManifestFileName)
	data, ok := readRegularFile(root, rel)
	if !ok {
		return false
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false
	}
	return manifest.CollectionInfo.Version == doc.Version
}

// readInstalledSidecar reads a sidecar's GALAXY.yml, warning only when a file
// fails to parse, and its provenance from provenanceFileName, else GALAXY.yml
// where older releases wrote it, so their git and url installs keep their kind.
func readInstalledSidecar(
	root *os.Root, cfg *config.Config, runtime *infra.Infra, infoName string,
) (GalaxyYAML, sidecarProvenance, bool) {
	rel := path.Join(collectionsDirName, infoName, galaxyYAMLFileName)
	data, ok := readRegularFile(root, rel)
	if !ok {
		return GalaxyYAML{}, sidecarProvenance{}, false
	}
	var doc GalaxyYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		runtime.Output.Warnf("Skipping sidecar %s: %v", displayPath(cfg, rel), err)
		return GalaxyYAML{}, sidecarProvenance{}, false
	}
	provRel := path.Join(collectionsDirName, infoName, provenanceFileName)
	if provData, ok := readRegularFile(root, provRel); ok {
		data, rel = provData, provRel
	}
	var prov sidecarProvenance
	if err := yaml.Unmarshal(data, &prov); err != nil {
		runtime.Output.Warnf("Skipping sidecar %s: %v", displayPath(cfg, rel), err)
		return GalaxyYAML{}, sidecarProvenance{}, false
	}
	return doc, prov, true
}

// readRegularFile reads rel through root only when Lstat says it is a regular
// file. The check must precede the read: opening a fifo blocks, and os.Root
// bounds where a path leads, not what sits at its end.
func readRegularFile(root *os.Root, rel string) ([]byte, bool) {
	info, err := root.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil, false
	}
	return data, true
}

// displayPath renders a tree-relative path as the OS-native absolute
// location an operator can act on, since a slash-separated path relative to a
// root nobody named tells them nothing about which tree it was in.
func displayPath(cfg *config.Config, rel string) string {
	return filepath.Join(cfg.DownloadPath, filepath.FromSlash(rel))
}

// installedRoleCount counts the roles under cfg.RolesPath carrying
// meta/.galaxy_install_info, which records no source, so the run can disclose
// them as unchecked. Every failure counts as zero: a disclosure never aborts.
func installedRoleCount(cfg *config.Config) int {
	root, err := os.OpenRoot(cfg.RolesPath)
	if err != nil {
		return 0
	}
	defer func() { _ = root.Close() }()
	dirs, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0
	}
	count := 0
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		if _, ok := readRegularFile(root, path.Join(dir.Name(), galaxyInstallInfoRel)); ok {
			count++
		}
	}
	return count
}

// reportInstalledGaps warns once per kind about what the tree-driven report
// cannot cover: git-sourced collections and installed roles.
func reportInstalledGaps(runtime *infra.Infra, scan installedScan, roles int) {
	if len(scan.skippedGit) > 0 {
		runtime.Output.Warnf(
			"not checked, installed from git and the tree records no ref to compare: %s; run lock to cover them",
			strings.Join(scan.skippedGit, ", "))
	}
	if roles > 0 {
		runtime.Output.Warnf(
			"not checked, %d installed role(s): meta/.galaxy_install_info records no source; run lock to cover them", roles)
	}
}

// errNoInstalledCollections is the failure with neither a lockfile nor a tree.
// It wraps helpers.ErrLockfileMissing so the exit code stays the missing
// lockfile's: the fallback adds no exit class.
func errNoInstalledCollections(lockPath, treePath string, cause error) error {
	return fmt.Errorf("%w: %s, and no collections installed at %s: %w",
		helpers.ErrLockfileMissing, lockPath, treePath, cause)
}

// isTreeAbsent reports whether err says the collections tree simply is not
// there, which is the one failure the caller turns into
// errNoInstalledCollections rather than propagating.
func isTreeAbsent(err error) bool { return errors.Is(err, fs.ErrNotExist) }

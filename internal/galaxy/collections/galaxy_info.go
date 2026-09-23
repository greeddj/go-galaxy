package collections

import (
	"bytes"
	"errors"
	"io/fs"
	"path"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// galaxyYAMLFileName is the sidecar file name inside a collection's .info
// directory, shared by writeGalaxyInfo and matchingInstalledRecord so the
// writer and the skip check can never disagree on it.
const galaxyYAMLFileName = "GALAXY.yml"

// provenanceFileName is the one file this tool adds to ansible's .info layout,
// holding a git or url install's provenance (see sidecarProvenance); ansible
// ignores it and removes it with the directory.
const provenanceFileName = "go-galaxy.yml"

// GalaxyYAML is GALAXY.yml held to exactly ansible-core's closed schema: one
// extra key and ansible discards the whole document. For a git or url install
// Server (and for url DownloadURL) names the repository or tarball.
type GalaxyYAML struct {
	DownloadURL string           `yaml:"download_url"`
	FormatVer   string           `yaml:"format_version"`
	Name        string           `yaml:"name"`
	Namespace   string           `yaml:"namespace"`
	Server      string           `yaml:"server"`
	Version     string           `yaml:"version"`
	VersionURL  string           `yaml:"version_url"`
	Signatures  galaxySignatures `yaml:"signatures"`
}

// galaxySignature is one entry of GALAXY.yml's signatures list, with exactly
// the keys ansible's schema admits there. ansible reads Signature by indexing
// the entry, so an entry without one is never kept.
type galaxySignature struct {
	PubkeyFingerprint string `yaml:"pubkey_fingerprint,omitempty"`
	PulpCreated       string `yaml:"pulp_created,omitempty"`
	Signature         string `yaml:"signature"`
	SigningService    string `yaml:"signing_service,omitempty"`
}

// galaxySignatures is GALAXY.yml's signatures list: it always renders as a
// list, since a null breaks `ansible-galaxy collection verify --offline`, and
// decoding never fails, dropping any odd entry rather than the whole document.
type galaxySignatures []galaxySignature

// UnmarshalYAML implements yaml.Unmarshaler; see galaxySignatures for the
// rule it applies.
func (s *galaxySignatures) UnmarshalYAML(node *yaml.Node) error {
	*s = nil
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range node.Content {
		var entry galaxySignature
		if item.Kind != yaml.MappingNode || item.Decode(&entry) != nil || entry.Signature == "" {
			continue
		}
		*s = append(*s, entry)
	}
	return nil
}

// sidecarSignatures converts a server's version-document signatures, of
// whatever shape, into GALAXY.yml's list through the same decode a sidecar
// read uses, so one rule decides what an entry has to be.
func sidecarSignatures(raw any) galaxySignatures {
	var node yaml.Node
	if err := node.Encode(raw); err != nil {
		return nil
	}
	var signatures galaxySignatures
	if err := signatures.UnmarshalYAML(&node); err != nil {
		return nil
	}
	return signatures
}

// sidecarProvenance is provenanceFileName's content: a git commit or a url
// sha256, which also tells outdated the install's kind. Its keys match what
// older releases wrote into GALAXY.yml, so readInstalledSidecar can fall back.
type sidecarProvenance struct {
	GitCommit string `yaml:"git_commit,omitempty"`
	URLSHA256 string `yaml:"url_sha256,omitempty"`
}

// buildProvenance builds col's provenance document, which is empty for a
// collection from a Galaxy server.
func buildProvenance(col collection) sidecarProvenance {
	if loc, err := col.gitLocator(); err == nil && col.isGit() {
		return sidecarProvenance{GitCommit: loc.Commit}
	}
	if loc, err := col.urlLocator(); err == nil && col.isURL() {
		return sidecarProvenance{URLSHA256: loc.SHA256}
	}
	return sidecarProvenance{}
}

// describes reports whether this document names col's exact identity, the
// fields buildGalaxyYAML fills from col, so a truncated or foreign sidecar is
// never taken as evidence of an install.
func (g GalaxyYAML) describes(col collection) bool {
	return g.Namespace == col.Namespace && g.Name == col.Name && g.Version == col.Version
}

// readGalaxyInfo parses the sidecar beside target and returns its bytes too,
// so the skip path can tell whether a re-render would change the file; any
// outcome short of a parsed document reports false.
func readGalaxyInfo(target installTarget) (GalaxyYAML, []byte, bool) {
	data, ok := readRegularFile(target.root, path.Join(target.info, galaxyYAMLFileName))
	if !ok {
		return GalaxyYAML{}, nil, false
	}
	var doc GalaxyYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GalaxyYAML{}, nil, false
	}
	return doc, data, true
}

// reconcileGalaxyInfo repairs a skipped install's .info documents from the
// store record: the provenance file first, then GALAXY.yml re-rendered with
// the resolving server. Failures only warn; the installed tree is correct.
func reconcileGalaxyInfo(runtime *infra.Infra, target installTarget, cfg *config.Config, col collection, state installedState) {
	if !reconcileProvenance(runtime, target, col) {
		return
	}
	doc := state.info
	doc.Server = buildGalaxyYAML(cfg, col, nil).Server
	data, err := yaml.Marshal(&doc)
	if err != nil {
		runtime.Output.Warnf("%s: cannot rebuild %s: %v", col.key(), galaxyYAMLFileName, err)
		return
	}
	if bytes.Equal(data, state.infoData) {
		return
	}
	if replaceInfoFile(runtime, target, col, galaxyYAMLFileName, data) {
		runtime.Output.Debugf("%s: %s rewritten to match the install record and ansible's schema", col.key(), galaxyYAMLFileName)
	}
}

// reconcileProvenance makes provenanceFileName hold what col's record
// implies, reporting whether it does. A Galaxy collection needs none and no
// stray file is looked for, since an install resets the whole .info directory.
func reconcileProvenance(runtime *infra.Infra, target installTarget, col collection) bool {
	prov := buildProvenance(col)
	if prov == (sidecarProvenance{}) {
		return true
	}
	data, err := yaml.Marshal(&prov)
	if err != nil {
		runtime.Output.Warnf("%s: cannot rebuild %s: %v", col.key(), provenanceFileName, err)
		return false
	}
	if have, ok := readRegularFile(target.root, path.Join(target.info, provenanceFileName)); ok && bytes.Equal(have, data) {
		return true
	}
	if !replaceInfoFile(runtime, target, col, provenanceFileName, data) {
		return false
	}
	runtime.Output.Debugf("%s: %s rewritten to match the install record", col.key(), provenanceFileName)
	return true
}

// replaceInfoFile is writeInfoFile for the skip path, where a failure is a
// warning rather than an error (see reconcileGalaxyInfo), reporting whether
// the file was written.
func replaceInfoFile(runtime *infra.Infra, target installTarget, col collection, name string, data []byte) bool {
	if err := writeInfoFile(target, name, data); err != nil {
		runtime.Output.Warnf("%s: cannot write %s: %v", col.key(), name, err)
		return false
	}
	return true
}

// writeInfoFile replaces name in target's .info directory with data, removing
// whatever sits at the name first so the write never goes through a planted
// symlink or hardlink, which os.Root alone cannot stop.
func writeInfoFile(target installTarget, name string, data []byte) error {
	rel := path.Join(target.info, name)
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	if err := target.root.WriteFile(rel, data, helpers.FileMod); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	return nil
}

// writeGalaxyInfo writes GALAXY.yml, and a git or url collection's provenance
// file, file by file rather than resetting .info, which would erase the
// extract marker; with meta nil it writes the identity fields only.
func writeGalaxyInfo(target installTarget, cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) error {
	if err := ensureInfoDir(target); err != nil {
		return err
	}
	g := buildGalaxyYAML(cfg, col, meta)
	data, err := yaml.Marshal(&g)
	if err != nil {
		return err
	}
	if err := writeInfoFile(target, galaxyYAMLFileName, data); err != nil {
		return err
	}
	prov := buildProvenance(col)
	if prov == (sidecarProvenance{}) {
		rel := path.Join(target.info, provenanceFileName)
		if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return classifyCollectionsRootError(target.root, target.info, err)
		}
		return nil
	}
	data, err = yaml.Marshal(&prov)
	if err != nil {
		return err
	}
	return writeInfoFile(target, provenanceFileName, data)
}

// ensureInfoDir makes target.info a real directory, leaving one that already
// is untouched; see writeGalaxyInfo for why anything else at that name is
// removed first.
func ensureInfoDir(target installTarget) error {
	info, err := target.root.Lstat(target.info)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		if err := target.root.RemoveAll(target.info); err != nil {
			return classifyCollectionsRootError(target.root, target.info, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	if err := target.root.MkdirAll(target.info, helpers.DirMod); err != nil {
		return classifyCollectionsRootError(target.root, target.info, err)
	}
	return nil
}

// buildGalaxyYAML builds col's GALAXY.yml with identity always from col. URLs
// from meta are cut by helpers.WithoutCredentials, needed because a cache hit
// with metadata reaches here without validateDownloadInputs ever running.
func buildGalaxyYAML(cfg *config.Config, col collection, meta *types.GalaxyCollectionVersionInfo) GalaxyYAML {
	g := GalaxyYAML{
		FormatVer: "1.0.0",
		Name:      col.Name,
		Namespace: col.Namespace,
		Server:    cfg.Server,
		Version:   col.Version,
	}
	if loc, err := col.gitLocator(); err == nil && col.isGit() {
		g.Server = helpers.WithoutCredentials(loc.URL)
		return g
	}
	if loc, err := col.urlLocator(); err == nil && col.isURL() {
		g.Server = helpers.WithoutCredentials(loc.URL)
		g.DownloadURL = helpers.WithoutCredentials(loc.URL)
		return g
	}
	// Record the server this collection resolved from, not the run's default:
	// outdated's tree-driven report asks the server this field names.
	if col.Source != "" {
		g.Server = col.Source
	}
	if meta != nil {
		g.DownloadURL = helpers.WithoutCredentials(meta.DownloadURL)
		g.Signatures = sidecarSignatures(meta.Signatures)
		g.VersionURL = helpers.WithoutCredentials(meta.Href)
	}
	return g
}

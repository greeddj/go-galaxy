package cleanup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// scanProjectWorkspace scans a project's collections workspace into the index.
// An absent or unrooted workspace is skipped, the latter with a warning quoting
// projectPath (checkout content); a scan IO error aborts the run.
func scanProjectWorkspace(
	out output.Printer,
	projectPath string,
	project store.ProjectRecord,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	ws, err := openProjectWorkspace(projectPath, project)
	if err != nil {
		out.Warnf("skipping project %q: %v; nothing under it was scanned or removed", projectPath, err)
		return nil
	}
	if ws.root == nil {
		return nil
	}
	defer func() { _ = ws.root.Close() }()

	if err := scanInstalledCollections(out, ws, index, byKey, deps); err != nil {
		return fmt.Errorf("failed to scan %q: %w", ws.path, err)
	}
	return nil
}

// scanInstalledCollections indexes only manifests at exactly
// ansible_collections/<ns>/<name>/MANIFEST.json, through the workspace's
// os.Root; a namespace or name entry that is not a real directory is skipped.
func scanInstalledCollections(
	out output.Printer,
	ws workspace,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nsEntries, err := fs.ReadDir(ws.fsys, "ansible_collections")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, nsEntry := range nsEntries {
		if !nsEntry.IsDir() {
			continue
		}
		if err := scanNamespaceDir(out, ws, nsEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// scanNamespaceDir scans every ansible_collections/<ns>/<name> directory for
// a MANIFEST.json, one namespace at a time.
func scanNamespaceDir(
	out output.Printer,
	ws workspace,
	ns string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	nameEntries, err := fs.ReadDir(ws.fsys, path.Join("ansible_collections", ns))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Vanished since the parent listing, e.g. under a concurrent run.
			return nil
		}
		return err
	}
	for _, nameEntry := range nameEntries {
		if !nameEntry.IsDir() {
			continue
		}
		if err := scanCollectionDir(out, ws, ns, nameEntry.Name(), index, byKey, deps); err != nil {
			return err
		}
	}
	return nil
}

// manifestIsRegularFile reports, by Lstat, whether the manifest at rel is a
// regular file, refusing a symlink, directory or fifo alike: reading a fifo
// would block. The raw Lstat error is returned so the caller can test it.
func manifestIsRegularFile(root *os.Root, rel string) (bool, error) {
	info, err := root.Lstat(rel)
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// scanCollectionDir probes ansible_collections/<ns>/<name>/MANIFEST.json and,
// if present, regular, and parseable, indexes the installed collection it
// describes.
func scanCollectionDir(
	out output.Printer,
	ws workspace,
	ns, name string,
	index map[string][]installedCollection,
	byKey map[string][]installedCollection,
	deps map[string]map[string]string,
) error {
	rel := path.Join("ansible_collections", ns, name, helpers.ManifestFileName)
	manifestPath := filepath.Join(ws.path, filepath.FromSlash(rel))

	regular, err := manifestIsRegularFile(ws.root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A <ns>/<name> directory without a MANIFEST.json is not an
			// installed collection.
			return nil
		}
		return err
	}
	if !regular {
		// A non-regular manifest identifies no collection: neither a root
		// nor a deletion candidate, so it is warned about and never opened.
		out.Warnf("skipping non-regular manifest at %q", manifestPath)
		return nil
	}

	manifest, err := readManifest(ws.root, rel, manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Vanished since the Lstat gate, e.g. under a concurrent run; no
			// test can force this race.
			return nil
		}
		if errors.Is(err, helpers.ErrCorruptManifest) {
			// An unparseable manifest identifies no collection, so its tree is
			// left alone; manifestPath holds unvalidated ns/name, hence %q.
			out.Warnf("skipping corrupt manifest at %q: %v", manifestPath, err)
			return nil
		}
		return err
	}
	record, key, ok, err := buildInstalledRecord(ws.path, manifestPath, ns, name, manifest)
	if err != nil {
		// manifestPath holds unvalidated ns/name, hence %q.
		out.Warnf("skipping install with unsafe identifier at %q: %v", manifestPath, err)
		return nil
	}
	if !ok {
		return nil
	}
	index[record.FQDN] = append(index[record.FQDN], record)
	byKey[key] = append(byKey[key], record)
	deps[key] = extractDeps(manifest)
	return nil
}

// readManifest reads and parses the MANIFEST.json at rel through root. A read
// error is returned as-is; a parse failure is helpers.ErrCorruptManifest
// naming display, the absolute path an operator can find.
func readManifest(root *os.Root, rel, display string) (types.GalaxyCollectionVersionInfoManifest, error) {
	data, err := root.ReadFile(rel)
	if err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, err
	}
	var manifest types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return types.GalaxyCollectionVersionInfoManifest{}, fmt.Errorf("%w at %s: %w", helpers.ErrCorruptManifest, display, err)
	}
	return manifest, nil
}

// buildInstalledRecord builds a record whose namespace and name are the walked
// directories, never manifest values, so no manifest can retarget a deletion;
// only the version is read from it, and all three must pass IsPathElement.
func buildInstalledRecord(
	collectionsPath string,
	manifestPath string,
	ns, name string,
	manifest types.GalaxyCollectionVersionInfoManifest,
) (installedCollection, string, bool, error) {
	version := manifest.CollectionInfo.Version
	if ns == "" || name == "" || version == "" {
		return installedCollection{}, "", false, nil
	}
	if !helpers.IsPathElement(ns) || !helpers.IsPathElement(name) || !helpers.IsPathElement(version) {
		return installedCollection{}, "", false, fmt.Errorf(
			"%w: ns=%q name=%q version=%q", helpers.ErrUnsafeCollectionIdentifier, ns, name, version,
		)
	}
	installPath := filepath.Dir(manifestPath)
	key := fmt.Sprintf("%s.%s@%s", ns, name, version)
	fqdn := fmt.Sprintf("%s.%s", ns, name)
	// A non-semver version stays indexed with a nil Parsed, which
	// selectInstalled skips under a real constraint.
	parsed, _ := semver.NewVersion(version)
	return installedCollection{
		Key:            key,
		FQDN:           fqdn,
		Namespace:      ns,
		Name:           name,
		Version:        version,
		InstallPath:    installPath,
		CollectionsDir: collectionsPath,
		Parsed:         parsed,
	}, key, true, nil
}

func extractDeps(manifest types.GalaxyCollectionVersionInfoManifest) map[string]string {
	if manifest.CollectionInfo.Dependencies != nil {
		return manifest.CollectionInfo.Dependencies
	}
	return map[string]string{}
}

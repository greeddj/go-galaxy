package cleanup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/rolebuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// installedRole is a role directory under a recorded roles path carrying this
// tool's extract marker, the only evidence this tool wrote it, joined with the
// snapshot's installed-role record for its path when one exists.
type installedRole struct {
	Name        string
	InstallPath string
	RolesDir    string
	Source      string
	Version     string
	ArtifactSHA string
	Deps        []string
}

// rolesByName holds every on-disk copy of a role name across the recorded
// roles paths: a name any project reaches keeps every copy, and an unreachable
// one loses every copy in one run.
type rolesByName map[string][]installedRole

// scanProjectRoles indexes the marked roles under a project's recorded roles
// path, through an os.Root at it. A record with no roles path is never scanned:
// guessing one would aim a delete at a directory nobody configured.
func scanProjectRoles(out output.Printer, projectPath string, project store.ProjectRecord, st *store.Store, byName rolesByName) error {
	if project.RolesPath == "" {
		out.Debugf("project %q: no roles path recorded; roles are not scanned", projectPath)
		return nil
	}
	root, err := os.OpenRoot(project.RolesPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		out.Warnf("skipping roles of project %q: %v; nothing under %q was scanned or removed", projectPath, err, project.RolesPath)
		return nil
	}
	defer func() { _ = root.Close() }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("failed to scan %q: %w", project.RolesPath, err)
	}
	records := recordsByInstallPath(st)
	for _, e := range entries {
		if inst, ok := scannedRole(root, project.RolesPath, e, records); ok {
			byName[inst.Name] = append(byName[inst.Name], inst)
		}
	}
	return nil
}

// scannedRole returns the installed role for a roles path entry that is a
// directory with a role install name and this tool's marker, joined with the
// snapshot record for its path when one exists.
func scannedRole(root *os.Root, rolesPath string, e fs.DirEntry, records map[string]store.InstalledRoleEntry) (installedRole, bool) {
	if !e.IsDir() || !helpers.IsRoleInstallName(e.Name()) {
		return installedRole{}, false
	}
	sha, ok := roleMarkerSHA(root, e.Name())
	if !ok {
		return installedRole{}, false
	}
	inst := installedRole{
		Name:        e.Name(),
		InstallPath: filepath.Join(rolesPath, e.Name()),
		RolesDir:    rolesPath,
		ArtifactSHA: sha,
	}
	if rec, ok := records[inst.InstallPath]; ok {
		inst.Source, inst.Version, inst.Deps = rec.Source, rec.Version, rec.Deps
		if rec.ArtifactSHA256 != "" {
			inst.ArtifactSHA = rec.ArtifactSHA256
		}
		return inst, true
	}
	// Without a record the dependencies come from the role's own meta, so
	// reachability never rests on the snapshot alone; the artifact, whose key
	// needs the record, is not purged.
	inst.Deps = installedRoleDeps(root, e.Name())
	return inst, true
}

// installedRoleDeps reads the install names a role's meta/main.yml and
// meta/requirements.yml depend on, under the install's own grammar; an
// unreadable meta yields no dependencies rather than a failed run.
func installedRoleDeps(root *os.Root, name string) []string {
	var specs []gitsource.RoleDependency
	meta := path.Join(name, "meta")
	if data, err := readRoleMeta(root, path.Join(meta, "main.yml"), path.Join(meta, "main.yaml")); data != nil && err == nil {
		if parsed, err := rolebuild.ParseMetaMain(data); err == nil {
			specs = append(specs, parsed.Dependencies...)
		}
	}
	if data, err := readRoleMeta(root, path.Join(meta, "requirements.yml"), path.Join(meta, "requirements.yaml")); data != nil && err == nil {
		if more, _, err := rolebuild.ParseMetaRequirements(data); err == nil {
			specs = append(specs, more...)
		}
	}
	var deps []string
	for _, spec := range specs {
		req, skip, err := requirements.ParseRoleDependency(spec)
		if err == nil && skip == requirements.DependencyInstalled {
			deps = append(deps, req.Name)
		}
	}
	return deps
}

// readRoleMeta reads the first of the candidate files that exists as a
// regular file under root, capped at the build metadata size; nil data and a
// nil error when none does.
func readRoleMeta(root *os.Root, candidates ...string) ([]byte, error) {
	for _, rel := range candidates {
		info, err := root.Stat(rel)
		if err != nil || !info.Mode().IsRegular() || info.Size() > helpers.BuildMetadataMaxBytes {
			continue
		}
		f, err := root.Open(rel)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, helpers.BuildMetadataMaxBytes))
		_ = f.Close()
		return data, err
	}
	return nil, nil
}

// recordsByInstallPath indexes the snapshot's installed-role records by the
// directory they name, the join key between a scanned directory and what
// the install recorded about it.
func recordsByInstallPath(st *store.Store) map[string]store.InstalledRoleEntry {
	out := make(map[string]store.InstalledRoleEntry)
	if st == nil {
		return out
	}
	for _, rec := range st.InstalledRolesSnapshot() {
		if rec.InstallPath != "" {
			out[rec.InstallPath] = rec
		}
	}
	return out
}

// roleMarkerSHA reports whether the role directory name under root holds an
// extract marker of this tool's, and the sha it names. Only a regular file
// with the marker prefix and a sha-shaped suffix counts.
func roleMarkerSHA(root *os.Root, name string) (string, bool) {
	entries, err := fs.ReadDir(root.FS(), name)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		sha, ok := strings.CutPrefix(e.Name(), helpers.ExtractMarkerPrefix)
		if ok && e.Type().IsRegular() && helpers.IsSHA256Hex(sha) {
			return sha, true
		}
	}
	return "", false
}

// markReachableRoles marks every role name a project's requirements reach:
// the roles it names, and transitively the install names each installed
// copy recorded as its dependencies.
func markReachableRoles(roots []string, byName rolesByName, reachable map[string]bool) {
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if reachable[name] {
			continue
		}
		reachable[name] = true
		for _, inst := range byName[name] {
			queue = append(queue, inst.Deps...)
		}
	}
}

// removeUnusedRoles removes every unreached role directory, its recorded
// artifact and its snapshot record, in sorted order for a repeatable partial
// result; only marked directories are ever in byName.
func removeUnusedRoles(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	st *store.Store,
	reachable map[string]bool,
	byName rolesByName,
) (int, error) {
	var removed int
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if err := ctx.Err(); err != nil {
			return removed, fmt.Errorf("cleanup stopped at role %s: %w", name, err)
		}
		if reachable[name] {
			continue
		}
		removed++
		if cfg.DryRun {
			runtime.Output.Printf("Would remove role %s", name)
			continue
		}
		for _, inst := range byName[name] {
			if err := removeRole(ctx, inst, backend.Artifacts()); err != nil {
				return removed, err
			}
		}
		runtime.Output.Printf("Removed role %s", name)
		if st != nil {
			st.DeleteInstalledRole(name)
		}
	}
	return removed, nil
}

// removeRole deletes one role directory through a fresh os.Root at its roles
// path, re-validating the name, and purges the recorded artifact; a fresh root
// refuses a roles path swapped in since the scan.
func removeRole(ctx context.Context, inst installedRole, artifacts cacheManager.ArtifactStore) error {
	if !helpers.IsRoleInstallName(inst.Name) {
		return fmt.Errorf("%w: role %q", helpers.ErrUnsafeRemovalPath, inst.Name)
	}
	root, err := os.OpenRoot(inst.RolesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(inst.Name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to remove role %s: %w", inst.InstallPath, err)
	}
	if inst.Source != "" && inst.Version != "" && artifacts != nil {
		_ = artifacts.Delete(ctx, helpers.ArtifactKey(inst.Source, helpers.RoleArtifactFilename(inst.Name, inst.Version)))
	}
	return nil
}

// roleKeepSHAs is the extracted-store keep set's role half: the artifact sha
// of every installed role that is not about to be removed.
func roleKeepSHAs(st *store.Store, reachable map[string]bool, byName rolesByName) map[string]bool {
	keep := make(map[string]bool)
	if st == nil {
		return keep
	}
	for name, sha := range st.InstalledRoleArtifactSHAs() {
		if _, scanned := byName[name]; scanned && !reachable[name] {
			continue
		}
		keep[sha] = true
	}
	return keep
}

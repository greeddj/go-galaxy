package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"slices"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/greeddj/go-galaxy/internal/safeout"
	"github.com/urfave/cli/v3"
)

// Tree returns the CLI command that prints the resolved dependency tree from
// the lockfile, one tree per requirements root, in the style of cargo tree.
func Tree() *cli.Command {
	return &cli.Command{
		Name:    "tree",
		Aliases: []string{"t"},
		Usage:   "Print the resolved dependency tree from the lockfile",
		Flags:   cliflags.LockInspectFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			reqPath := c.String("requirements-file")
			lockPath := lockfile.ResolveDefaultPath(reqPath, c.String("lock-file"))
			lf, err := lockfile.LoadRequired(lockPath)
			if err != nil {
				return err
			}
			roots, roleRoots, err := loadRootFQDNs(reqPath, lf)
			if err != nil {
				return err
			}
			printTree(os.Stdout, reqPath, lf, roots)
			printRoleTree(os.Stdout, lf, roleRoots)
			return nil
		},
	}
}

// loadRootFQDNs lists the collection and role roots of the requirements file.
// A git or url entry's roots are the lockfile entries locked from it; with none,
// it is listed by its name or locator so the tree shows it as missing.
func loadRootFQDNs(reqPath string, lf *lockfile.File) ([]string, []string, error) {
	file, err := requirements.Load(reqPath, "")
	if err != nil {
		return nil, nil, fmt.Errorf("load requirements %s: %w", reqPath, err)
	}
	out := make([]string, 0, len(file.Collections))
	for _, r := range file.Collections {
		if r.IsGit() {
			out = append(out, gitRootFQDNs(r, lf)...)
			continue
		}
		if r.IsURL() {
			out = append(out, urlRootFQDNs(r, lf)...)
			continue
		}
		out = append(out, fmt.Sprintf("%s.%s", r.Namespace, r.Name))
	}
	roles := make([]string, 0, len(file.Roles))
	for _, r := range file.Roles {
		roles = append(roles, r.Name)
	}
	return out, roles, nil
}

func gitRootFQDNs(r requirements.CollectionRequirement, lf *lockfile.File) []string {
	var out []string
	if lf != nil {
		for _, e := range lf.Collections {
			if !e.IsGit() || e.Source != r.Source || !gitSubdirWithin(e.Subdir, r.Subdir) {
				continue
			}
			if r.Name != "" && e.Name != r.Namespace+"."+r.Name {
				continue
			}
			out = append(out, e.Name)
		}
	}
	if len(out) == 0 {
		if r.Name != "" {
			return []string{r.Namespace + "." + r.Name}
		}
		return []string{gitsource.Locator{URL: r.Source, Subdir: r.Subdir}.String()}
	}
	return out
}

// urlRootFQDNs lists the lockfile entry locked from a url entry's URL, or the
// entry's locator text when there is none, so the tree shows it as missing.
func urlRootFQDNs(r requirements.CollectionRequirement, lf *lockfile.File) []string {
	if lf != nil {
		for _, e := range lf.Collections {
			if e.IsURL() && e.Source == r.Source {
				return []string{e.Name}
			}
		}
	}
	return []string{urlsource.Locator{URL: r.Source}.String()}
}

// gitSubdirWithin reports whether a locked entry's subdir is the requirement's
// own subdir or an immediate child of it.
func gitSubdirWithin(entrySubdir, rootSubdir string) bool {
	if entrySubdir == rootSubdir {
		return true
	}
	parent := path.Dir(entrySubdir)
	if parent == "." {
		parent = ""
	}
	return parent == rootSubdir
}

// printTree writes the requirements path as the header, then one tree per
// root. w is wrapped in safeout because a lockfile may be hand-edited or
// untrusted, and its fields are otherwise printed verbatim.
func printTree(w io.Writer, reqPath string, lf *lockfile.File, roots []string) {
	w = safeout.NewWriter(w)
	byFQDN := make(map[string]lockfile.Entry, len(lf.Collections))
	for _, e := range lf.Collections {
		byFQDN[e.Name] = e
	}
	sortedRoots := slices.Sorted(slices.Values(roots))

	_, _ = fmt.Fprintln(w, reqPath)
	for i, root := range sortedRoots {
		isLast := i == len(sortedRoots)-1
		walkTree(w, byFQDN, root, "", isLast, make(map[string]bool))
	}
}

func walkTree(w io.Writer, by map[string]lockfile.Entry, fqdn, prefix string, isLast bool, seen map[string]bool) {
	branch := "├── "
	cont := "│   "
	if isLast {
		branch = "└── "
		cont = "    "
	}
	entry, ok := by[fqdn]
	if !ok {
		_, _ = fmt.Fprintf(w, "%s%s%s (missing in lockfile)\n", prefix, branch, fqdn)
		return
	}
	if seen[fqdn] {
		_, _ = fmt.Fprintf(w, "%s%s%s %s (*)\n", prefix, branch, fqdn, entry.Version)
		return
	}
	seen[fqdn] = true
	_, _ = fmt.Fprintf(w, "%s%s%s %s%s\n", prefix, branch, fqdn, entry.Version, gitOrigin(entry))

	deps := slices.Sorted(slices.Values(entry.Deps))
	for i, dep := range deps {
		walkTree(w, by, dep, prefix+cont, i == len(deps)-1, seen)
	}
}

// gitOrigin renders a git or url entry's provenance for the tree: the
// repository, the subdir when one is set, and the commit - or the tarball
// URL and its sha256 prefix. Empty for a Galaxy entry.
func gitOrigin(entry lockfile.Entry) string {
	if entry.IsURL() {
		return fmt.Sprintf(" (url %s sha256:%s)", helpers.WithoutCredentials(entry.Source), entry.SHA256[:helpers.ArtifactKeyFingerprintLen])
	}
	if !entry.IsGit() {
		return ""
	}
	subdir := ""
	if entry.Subdir != "" {
		subdir = "#" + entry.Subdir
	}
	return fmt.Sprintf(" (git %s%s @%s)", entry.Source, subdir, entry.Commit)
}

// printRoleTree writes the roles half under a "roles:" header, one tree per
// root, and nothing when neither file has roles. w is wrapped in safeout for
// the reason printTree's is.
func printRoleTree(w io.Writer, lf *lockfile.File, roots []string) {
	if len(lf.Roles) == 0 && len(roots) == 0 {
		return
	}
	w = safeout.NewWriter(w)
	byName := make(map[string]lockfile.RoleEntry, len(lf.Roles))
	for _, e := range lf.Roles {
		byName[e.Name] = e
	}
	sortedRoots := slices.Sorted(slices.Values(roots))
	_, _ = fmt.Fprintln(w, "roles:")
	for i, root := range sortedRoots {
		walkRoleTree(w, byName, root, "", i == len(sortedRoots)-1, make(map[string]bool))
	}
}

func walkRoleTree(w io.Writer, by map[string]lockfile.RoleEntry, name, prefix string, isLast bool, seen map[string]bool) {
	branch := "├── "
	cont := "│   "
	if isLast {
		branch = "└── "
		cont = "    "
	}
	entry, ok := by[name]
	if !ok {
		_, _ = fmt.Fprintf(w, "%s%s%s (missing in lockfile)\n", prefix, branch, name)
		return
	}
	if seen[name] {
		_, _ = fmt.Fprintf(w, "%s%s%s %s (*)\n", prefix, branch, name, entry.Version)
		return
	}
	seen[name] = true
	_, _ = fmt.Fprintf(w, "%s%s%s %s%s\n", prefix, branch, name, entry.Version, roleOrigin(entry))
	deps := slices.Sorted(slices.Values(entry.Deps))
	for i, dep := range deps {
		walkRoleTree(w, by, dep, prefix+cont, i == len(deps)-1, seen)
	}
}

// roleOrigin renders a role entry's provenance for the tree: the repository
// and commit for a git role, the Galaxy name and the repository it was
// imported from for a Galaxy role.
func roleOrigin(entry lockfile.RoleEntry) string {
	if entry.IsURL() {
		return fmt.Sprintf(" (url %s sha256:%s)", helpers.WithoutCredentials(entry.Source), entry.SHA256[:helpers.ArtifactKeyFingerprintLen])
	}
	if entry.IsGit() {
		return fmt.Sprintf(" (git %s @%s)", entry.Source, entry.Commit)
	}
	return fmt.Sprintf(" (galaxy %s via %s @%s)", entry.Galaxy, entry.Repository, entry.Commit)
}

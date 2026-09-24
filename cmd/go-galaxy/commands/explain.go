package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/greeddj/go-galaxy/internal/safeout"
	"github.com/urfave/cli/v3"
)

var (
	// errExplainNoTarget wraps helpers.ErrMissingArgument, so explain run with
	// no name exits as the usage error it is, the class an argument explain
	// does not take already exits with.
	errExplainNoTarget = fmt.Errorf("%w: explain takes one, a collection name (namespace.name) or a role name",
		helpers.ErrMissingArgument)
	// errExplainNotFound wraps no sentinel, so it exits 1 (generic): the lockfile
	// loaded and is valid, so not 6, and explain looks a name up without judging
	// its shape, so not 2.
	errExplainNotFound = errors.New("collection or role not found in lockfile")
)

// Explain returns the CLI command that prints why a particular collection
// version was chosen and which other collections depend on it.
func Explain() *cli.Command {
	return &cli.Command{
		Name:         "explain",
		Aliases:      []string{"why"},
		Usage:        "Explain why a collection or role was resolved to its locked version",
		ArgsUsage:    "<namespace.name | role name>",
		Flags:        cliflags.LockInspectFlags(),
		ArgValidator: explainArguments,
		Action: func(_ context.Context, c *cli.Command) error {
			target := c.Args().First()
			reqPath, warning := config.RequirementsPath(c)
			if warning != "" {
				progress.Warnf("%s", warning)
			}
			lockPath := lockfile.ResolveDefaultPath(reqPath, c.String("lock-file"))
			lf, err := lockfile.LoadRequired(lockPath)
			if err != nil {
				return err
			}
			roots, roleRoots, _ := loadRootFQDNs(reqPath, lf)
			rootSet := make(map[string]bool, len(roots))
			for _, r := range roots {
				rootSet[r] = true
			}
			roleRootSet := make(map[string]bool, len(roleRoots))
			for _, r := range roleRoots {
				roleRootSet[r] = true
			}
			return printExplain(os.Stdout, lf, target, filepath.Base(reqPath), rootSet, roleRootSet)
		},
	}
}

// explainArguments replaces the root's NoArguments for explain and demands one
// target. Extra arguments are refused by name rather than dropped, because
// "explain a b" answering only a would read as an answer about both.
func explainArguments(_ context.Context, c *cli.Command) error {
	args := c.Args().Slice()
	switch {
	case len(args) == 0:
		return errExplainNoTarget
	case len(args) > 1:
		return unexpectedArguments(c, args[1:], "one")
	default:
		return nil
	}
}

// printExplain writes why target was locked and what depends on it; a name that
// is both a collection and a role prints both, the collection first. rootLabel
// names the requirements file a root is required by; w is wrapped in safeout.
func printExplain(w io.Writer, lf *lockfile.File, target, rootLabel string, roots, roleRoots map[string]bool) error {
	w = safeout.NewWriter(w)
	entry, rdeps, found := findExplainTarget(lf, target)
	role, roleRdeps, roleFound := findExplainRole(lf, target)
	if !found && !roleFound {
		return fmt.Errorf("%w: %s", errExplainNotFound, target)
	}
	if found {
		printEntryHeader(w, entry)
		printRequiredBy(w, target, rootLabel, rdeps, roots)
		printDepends(w, entry)
	}
	if roleFound {
		printRoleHeader(w, role)
		printRoleRequiredBy(w, role, rootLabel, roleRdeps, roleRoots)
		printRoleDepends(w, role)
	}
	return nil
}

// findExplainRole matches target against the roles list by install name or
// Galaxy name, then collects the roles whose deps name the match's install
// name, since role deps hold install names, not the Galaxy name typed.
func findExplainRole(lf *lockfile.File, target string) (lockfile.RoleEntry, []lockfile.RoleEntry, bool) {
	var entry lockfile.RoleEntry
	found := false
	for _, e := range lf.Roles {
		if e.Name == target || e.Galaxy == target {
			entry = e
			found = true
		}
	}
	rdeps := make([]lockfile.RoleEntry, 0)
	if !found {
		return entry, rdeps, false
	}
	for _, e := range lf.Roles {
		if slices.Contains(e.Deps, entry.Name) {
			rdeps = append(rdeps, e)
		}
	}
	return entry, rdeps, true
}

func printRoleHeader(w io.Writer, entry lockfile.RoleEntry) {
	_, _ = fmt.Fprintf(w, "role %s %s\n", entry.Name, entry.Version)
	_, _ = fmt.Fprintf(w, "  type       : %s\n", entry.Type)
	if entry.Galaxy != "" {
		_, _ = fmt.Fprintf(w, "  galaxy     : %s\n", entry.Galaxy)
	}
	_, _ = fmt.Fprintf(w, "  source     : %s\n", entry.Source)
	if entry.Repository != "" {
		_, _ = fmt.Fprintf(w, "  repository : %s\n", entry.Repository)
	}
	if entry.Ref != "" {
		_, _ = fmt.Fprintf(w, "  ref        : %s\n", entry.Ref)
	}
	if entry.Commit != "" {
		_, _ = fmt.Fprintf(w, "  commit     : %s\n", entry.Commit)
	}
	if entry.SHA256 != "" {
		_, _ = fmt.Fprintf(w, "  sha256     : %s\n", entry.SHA256)
	}
}

func printRoleRequiredBy(w io.Writer, entry lockfile.RoleEntry, rootLabel string, rdeps []lockfile.RoleEntry, roots map[string]bool) {
	_, _ = fmt.Fprintln(w, "  required by:")
	if roots[entry.Name] {
		_, _ = fmt.Fprintln(w, "    - "+rootLabel+" (root)")
	}
	slices.SortFunc(rdeps, func(a, b lockfile.RoleEntry) int { return strings.Compare(a.Name, b.Name) })
	for _, r := range rdeps {
		_, _ = fmt.Fprintf(w, "    - role %s %s\n", r.Name, r.Version)
	}
	if !roots[entry.Name] && len(rdeps) == 0 {
		_, _ = fmt.Fprintln(w, "    - (no parents - orphan in lockfile)")
	}
}

func printRoleDepends(w io.Writer, entry lockfile.RoleEntry) {
	if len(entry.Deps) == 0 {
		return
	}
	deps := slices.Sorted(slices.Values(entry.Deps))
	_, _ = fmt.Fprintln(w, "  depends on:")
	for _, d := range deps {
		_, _ = fmt.Fprintf(w, "    - role %s\n", d)
	}
}

func findExplainTarget(lf *lockfile.File, target string) (lockfile.Entry, []lockfile.Entry, bool) {
	var entry lockfile.Entry
	found := false
	rdeps := make([]lockfile.Entry, 0)
	for _, e := range lf.Collections {
		if e.Name == target {
			entry = e
			found = true
		}
		if slices.Contains(e.Deps, target) {
			rdeps = append(rdeps, e)
		}
	}
	return entry, rdeps, found
}

func printEntryHeader(w io.Writer, entry lockfile.Entry) {
	_, _ = fmt.Fprintf(w, "%s %s\n", entry.Name, entry.Version)
	if entry.Type != "" {
		_, _ = fmt.Fprintf(w, "  type   : %s\n", entry.Type)
	}
	if entry.Source != "" {
		_, _ = fmt.Fprintf(w, "  source : %s\n", entry.Source)
	}
	if entry.Ref != "" {
		_, _ = fmt.Fprintf(w, "  ref    : %s\n", entry.Ref)
	}
	if entry.Commit != "" {
		_, _ = fmt.Fprintf(w, "  commit : %s\n", entry.Commit)
	}
	if entry.Subdir != "" {
		_, _ = fmt.Fprintf(w, "  subdir : %s\n", entry.Subdir)
	}
	if entry.SHA256 != "" {
		_, _ = fmt.Fprintf(w, "  sha256 : %s\n", entry.SHA256)
	}
}

func printRequiredBy(w io.Writer, target, rootLabel string, rdeps []lockfile.Entry, roots map[string]bool) {
	_, _ = fmt.Fprintln(w, "  required by:")
	if roots[target] {
		_, _ = fmt.Fprintln(w, "    - "+rootLabel+" (root)")
	}
	slices.SortFunc(rdeps, func(a, b lockfile.Entry) int { return strings.Compare(a.Name, b.Name) })
	for _, r := range rdeps {
		_, _ = fmt.Fprintf(w, "    - %s %s\n", r.Name, r.Version)
	}
	if !roots[target] && len(rdeps) == 0 {
		_, _ = fmt.Fprintln(w, "    - (no parents - orphan in lockfile)")
	}
}

func printDepends(w io.Writer, entry lockfile.Entry) {
	if len(entry.Deps) == 0 {
		return
	}
	deps := make([]string, len(entry.Deps))
	copy(deps, entry.Deps)
	slices.Sort(deps)
	_, _ = fmt.Fprintln(w, "  depends on:")
	for _, d := range deps {
		_, _ = fmt.Fprintf(w, "    - %s\n", d)
	}
}

// Package lockfile reads, validates and writes galaxy.lock, which pins every
// collection and role so a frozen install reads only the cache. The schema
// (1 to 4) is derived from the entries, so an older binary refuses a newer shape.
package lockfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"go.yaml.in/yaml/v3"
)

var errNilFile = errors.New("lockfile: nil File")

// SchemaVersion is the lockfile schema of a file without git entries, and
// the lowest one Load accepts. Bumping it requires migration.
const SchemaVersion = 1

// SchemaVersionGit is the schema of a file carrying at least one git entry.
// It differs from SchemaVersion only in that an entry may carry Type, Ref,
// Commit and Subdir; SchemaVersionFor picks it from the entries.
const SchemaVersionGit = 2

// TypeGit is the Entry.Type of a collection built from a git source, TypeURL
// of one downloaded from a tarball URL. An empty Type is a Galaxy entry; no
// other value is accepted.
const (
	TypeGit = "git"
	TypeURL = "url"
)

// SchemaVersionURL is the schema of a file carrying at least one url entry -
// a collection or a role pinned to a tarball URL by its sha256.
// SchemaVersionFor picks it from the entries.
const SchemaVersionURL = 4

// DefaultName is the conventional lockfile name beside the requirements file,
// galaxy.toml or requirements.yml, whichever the run read.
const DefaultName = "galaxy.lock"

// Entry is one pinned collection: a Galaxy entry pins version and SHA256, a
// git entry (TypeGit) the commit and no SHA256, since a rebuild's gzip bytes
// depend on the toolchain; a url entry (TypeURL) requires the origin's SHA256.
type Entry struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type,omitempty"`
	Version string   `yaml:"version"`
	Source  string   `yaml:"source"`
	Ref     string   `yaml:"ref,omitempty"`
	Commit  string   `yaml:"commit,omitempty"`
	Subdir  string   `yaml:"subdir,omitempty"`
	SHA256  string   `yaml:"sha256,omitempty"`
	Deps    []string `yaml:"deps,omitempty"`
}

// IsGit reports whether the entry pins a git source.
func (e Entry) IsGit() bool { return e.Type == TypeGit }

// IsURL reports whether the entry pins a url source.
func (e Entry) IsURL() bool { return e.Type == TypeURL }

// SchemaVersionFor returns the schema a file holding entries and roles is
// written with, the highest feature present winning: SchemaVersionURL, then
// SchemaVersionRoles, then SchemaVersionGit, else SchemaVersion.
func SchemaVersionFor(entries []Entry, roles []RoleEntry) int {
	switch {
	case anyURLEntry(entries, roles):
		return SchemaVersionURL
	case len(roles) > 0:
		return SchemaVersionRoles
	case slices.ContainsFunc(entries, Entry.IsGit):
		return SchemaVersionGit
	default:
		return SchemaVersion
	}
}

// anyURLEntry reports whether any collection or role entry is a url entry.
func anyURLEntry(entries []Entry, roles []RoleEntry) bool {
	return slices.ContainsFunc(entries, Entry.IsURL) || slices.ContainsFunc(roles, RoleEntry.IsURL)
}

// File is the on-disk lockfile structure.
type File struct {
	// Server is provenance: a source-less entry takes its Source from the run's
	// cfg.Server, not from here. Hash covers it and Compare reports it via
	// Diff.Server, so a server_list change that moves the default is drift.
	Server      string  `yaml:"server,omitempty"`
	Collections []Entry `yaml:"collections"`
	// Roles is every role the run installs, pinned to its commit; absent
	// from a file without roles, so such a file carries no roles key at all.
	Roles         []RoleEntry `yaml:"roles,omitempty"`
	SchemaVersion int         `yaml:"schema_version"`
}

// ResolveDefaultPath returns the lockfile path. If override is set, that
// path is used. Otherwise DefaultName is placed next to the requirements
// file (or in cwd if requirements path is empty).
func ResolveDefaultPath(requirementsFile, override string) string {
	if override != "" {
		return override
	}
	if requirementsFile == "" {
		return DefaultName
	}
	return filepath.Join(filepath.Dir(requirementsFile), DefaultName)
}

// Load parses and validates a lockfile. Every error either satisfies
// IsNotExist or wraps helpers.ErrLockfileInvalid, never both and never neither:
// LoadRequired, outdated and lock --dry-run tell absence from breakage by it.
func Load(path string) (*File, error) {
	//nolint:gosec // path is user-provided lockfile location.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// %s, not %w: the fs.ErrNotExist guard above already keeps the two arms
		// exclusive, and nothing branches on the underlying errno.
		return nil, fmt.Errorf("%w: %s", helpers.ErrLockfileInvalid, err.Error())
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: %s", helpers.ErrLockfileInvalid, err.Error())
	}
	if f.SchemaVersion == 0 {
		return nil, fmt.Errorf("%w: missing schema_version", helpers.ErrLockfileInvalid)
	}
	if f.SchemaVersion < SchemaVersion || f.SchemaVersion > SchemaVersionURL {
		return nil, fmt.Errorf("%w: schema_version=%d, supported=%d through %d",
			helpers.ErrLockfileInvalid, f.SchemaVersion, SchemaVersion, SchemaVersionURL)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Save writes the lockfile to disk in canonical form. It canonicalizes a copy
// so a caller that keeps using f after Save never sees its entries reordered.
func Save(path string, f *File) error {
	if f == nil {
		return errNilFile
	}
	data, err := marshal(f.canonicalClone())
	if err != nil {
		return err
	}
	return helpers.WriteFileAtomic(path, data)
}

// Hash returns a stable SHA256 hex of the canonical lockfile bytes.
func (f *File) Hash() (string, error) {
	clone := f.canonicalClone()
	data, err := marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// lockfileIndent is the indent width Save writes and Hash digests; yaml's own
// default is four.
const lockfileIndent = 2

// marshal renders f as the one byte sequence Save writes and Hash digests,
// so the indent is part of what `go-galaxy hash` prints: changing it changes
// every CI cache key built on a lockfile.
func marshal(f *File) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(lockfileIndent)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// canonicalClone returns a canonicalized deep copy of f. Only the slices that
// canonicalize sorts are copied - the Collections slice and each entry's Deps
// slice - so Hash stays allocation-light while never mutating the receiver.
func (f *File) canonicalClone() *File {
	clone := *f
	clone.Collections = make([]Entry, len(f.Collections))
	copy(clone.Collections, f.Collections)
	for i := range clone.Collections {
		src := f.Collections[i].Deps
		if len(src) == 0 {
			continue
		}
		deps := make([]string, len(src))
		copy(deps, src)
		clone.Collections[i].Deps = deps
	}
	clone.Roles = cloneRoles(f.Roles)
	canonicalize(&clone)
	return &clone
}

// validate rejects a duplicate or malformed name, a non-exact version (a
// --frozen install would then take the server's highest), a source with
// userinfo, a malformed git or url pin, and an entry its schema predates.
func (f *File) validate() error {
	seen := make(map[string]struct{}, len(f.Collections))
	for _, e := range f.Collections {
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate collection name %q", helpers.ErrLockfileInvalid, e.Name)
		}
		seen[e.Name] = struct{}{}
		// Checked before the version because the version message prints the
		// name, and a name carrying a newline would forge extra output lines.
		if err := checkEntryName(e); err != nil {
			return err
		}
		if !helpers.IsExactVersion(e.Version) {
			return fmt.Errorf("%w: %s: version %q is not an exact version", helpers.ErrLockfileInvalid, e.Name, e.Version)
		}
		// The source itself is never printed: it is what carries the password,
		// and this message reaches stderr and a CI log. The name is safe to
		// print by the check above.
		if sourceHasUserinfo(e.Source) {
			return fmt.Errorf("%w: %s: %w", helpers.ErrLockfileInvalid, e.Name, helpers.ErrGalaxyServerURLUserinfo)
		}
		if err := validateEntryType(e, f.SchemaVersion); err != nil {
			return err
		}
	}
	return f.validateRoles()
}

// checkEntryName holds a url entry's name, read from its artifact's own
// MANIFEST.json, to helpers.IsURLCollectionNamePart, and every other entry to
// the Galaxy alphabet a server-resolved name has already passed.
func checkEntryName(e Entry) error {
	if e.IsURL() {
		namespace, name, ok := helpers.SplitFQDN(e.Name)
		if !ok || !helpers.IsURLCollectionNamePart(namespace) || !helpers.IsURLCollectionNamePart(name) {
			return fmt.Errorf("%w: collection name %q is not <namespace>.<name> in the form ^[A-Za-z0-9_]+$",
				helpers.ErrLockfileInvalid, e.Name)
		}
		return nil
	}
	if !helpers.IsCollectionName(e.Name) {
		return fmt.Errorf("%w: collection name %q is not <namespace>.<name> in the form ^[a-z][a-z0-9_]*$",
			helpers.ErrLockfileInvalid, e.Name)
	}
	return nil
}

// validateEntryType judges the fields that set a git or url entry apart from
// a Galaxy one, and the minimum schema each needs. Sources are re-parsed and
// must round-trip unchanged, since a lockfile is repository content.
func validateEntryType(e Entry, schema int) error {
	var minSchema int
	var problem func(Entry) string
	switch e.Type {
	case "":
		if e.Ref != "" || e.Commit != "" || e.Subdir != "" {
			return fmt.Errorf("%w: %s: ref, commit and subdir belong to a git entry (type: git)", helpers.ErrLockfileInvalid, e.Name)
		}
		return nil
	case TypeGit:
		minSchema, problem = SchemaVersionGit, gitEntryProblem
	case TypeURL:
		minSchema, problem = SchemaVersionURL, urlEntryProblem
	default:
		return fmt.Errorf("%w: %s: unsupported entry type %q", helpers.ErrLockfileInvalid, e.Name, e.Type)
	}
	if schema < minSchema {
		return fmt.Errorf("%w: %s: a %s entry requires schema_version %d", helpers.ErrLockfileInvalid, e.Name, e.Type, minSchema)
	}
	if reason := problem(e); reason != "" {
		return fmt.Errorf("%w: %s: %s", helpers.ErrLockfileInvalid, e.Name, reason)
	}
	return nil
}

// gitEntryProblem returns why a git entry's source, ref, commit, subdir or
// sha256 is refused, or "" when every field is canonical.
func gitEntryProblem(e Entry) string {
	if u, err := gitsource.ParseURL(e.Source); err != nil || u.String() != e.Source {
		return "source is not a canonical git repository URL"
	}
	if ref, err := gitsource.ParseRef(e.Ref); err != nil || e.Ref == "" || ref.Name != e.Ref {
		return fmt.Sprintf("ref %q is not a canonical git ref", e.Ref)
	}
	if !gitsource.IsCommitHash(e.Commit) {
		return fmt.Sprintf("commit %q is not a lowercase 40-hex commit", e.Commit)
	}
	if subdir, err := gitsource.ParseSubdir(e.Subdir); err != nil || subdir != e.Subdir {
		return fmt.Sprintf("subdir %q is not a canonical subdir", e.Subdir)
	}
	if e.SHA256 != "" {
		return "a git entry carries no sha256"
	}
	return ""
}

// urlEntryProblem returns why a url entry's source, sha256 or leftover git
// fields are refused, or "" when every field is canonical.
func urlEntryProblem(e Entry) string {
	if u, err := urlsource.ParseURL(e.Source); err != nil || u.String() != e.Source {
		return "source is not a canonical tarball URL"
	}
	if !helpers.IsSHA256Hex(e.SHA256) {
		return fmt.Sprintf("sha256 %q is not a lowercase 64-hex digest", e.SHA256)
	}
	if e.Ref != "" || e.Commit != "" || e.Subdir != "" {
		return "ref, commit and subdir belong to a git entry"
	}
	return ""
}

// sourceHasUserinfo is the lockfile's half of requirements.checkSourceUserinfo:
// net/http turns URL userinfo into Basic auth that displaces the operator's
// token. A bare server_list id has no scheme or host and is left alone.
func sourceHasUserinfo(source string) bool {
	if source == "" {
		return false
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return parsed.User != nil
}

// IsNotExist reports whether err indicates the lockfile is missing.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// LoadRequired is Load for a command that cannot proceed without the file:
// absence becomes helpers.ErrLockfileMissing (exit 6), since a bare
// fs.ErrNotExist exits 2. A caller that falls back on absence uses Load.
func LoadRequired(path string) (*File, error) {
	f, err := Load(path)
	switch {
	case err == nil:
		return f, nil
	case IsNotExist(err):
		return nil, fmt.Errorf("%w: %s", helpers.ErrLockfileMissing, path)
	default:
		return nil, err
	}
}

// canonicalize sorts the entries, roles and their deps, and sets the schema
// from the content unconditionally: it is never a value a producer chooses.
func canonicalize(f *File) {
	f.SchemaVersion = SchemaVersionFor(f.Collections, f.Roles)
	slices.SortFunc(f.Collections, func(a, b Entry) int {
		return strings.Compare(a.Name, b.Name)
	})
	for i := range f.Collections {
		slices.Sort(f.Collections[i].Deps)
	}
	canonicalizeRoles(f.Roles)
}

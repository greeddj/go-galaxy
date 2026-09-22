// Package lockfile reads and writes go-galaxy lockfiles. The lockfile pins
// every transitive collection to an exact version with a SHA256 - or, for a
// collection built from a git source, to the commit it was built from - so
// CI runs are reproducible and hermetic: once a lockfile exists, install only
// reads the cache, never the Galaxy API or the git remote.
//
// Four schema versions are written and all four are read. Schema 1 is the
// Galaxy-only shape every lockfile had before git sources existed; schema 2
// adds the git fields to an entry and is written exactly when a file carries
// at least one git entry; schema 3 adds the roles list and is written
// exactly when a file carries at least one role; schema 4 admits the url
// entry shape - a collection or role pinned to a tarball URL by its sha256 -
// and is written exactly when a file carries one. canonicalize decides among
// them from the entries alone, so a project with no git source, role or url
// source keeps producing a schema-1 file that every release reads, and a
// project that drops its last git source, role or url source goes back on
// its next lock. An older binary reading a newer file refuses it loudly
// instead of installing a git or url entry's source as if it were a Galaxy
// server, or ignoring a roles list it does not know.
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

// DefaultName is the conventional lockfile name beside requirements.yml.
const DefaultName = "galaxy.lock"

// Entry is a single pinned collection in the lockfile. A Galaxy entry pins a
// version and the artifact's SHA256 under the server it came from; a git
// entry (Type == TypeGit) pins the commit the collection was built from,
// with Source naming the repository URL, Ref the branch, tag or commit the
// requirements file asked for, and Subdir the collection's directory inside
// the repository. A git entry carries no SHA256: the artifact is rebuilt
// deterministically from the commit, and the gzip bytes of that rebuild
// depend on the toolchain that produced them, so a digest over them would
// fail a frozen install for no reason the operator could act on. A url entry
// (Type == TypeURL) is the opposite case and its SHA256 is required: Source
// names the tarball URL, the artifact is the origin's own bytes rather than
// a rebuild, and the digest is the whole pin - there is no ref, commit or
// subdir dimension to carry.
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
// written with, the highest feature present winning: SchemaVersionURL when
// any collection or role entry is a url entry, else SchemaVersionRoles when
// there is any role, else SchemaVersionGit when any entry is a git entry,
// else SchemaVersion.
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
	// Server records the Galaxy server this lockfile was generated against -
	// provenance for a human reviewing a committed lockfile, never a source
	// of truth an entry's own resolution defers to: indexLockfile defaults a
	// source-less entry's Source from the consuming RUN's own cfg.Server, not
	// from this field. It is nonetheless part of the file's identity: Hash
	// covers it and Compare reports a change to it via Diff.Server, so a
	// server_list reorder or edit that changes the effective default server
	// counts as drift the same way a changed pin does, even when every
	// collection entry is otherwise untouched.
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

// Load parses a lockfile from disk. Every error it returns means exactly one
// of two things, and the two are mutually exclusive by construction: either
// the file is not there (IsNotExist(err) is true - the original
// fs.ErrNotExist, or an OS-specific variant like ENOTDIR that also satisfies
// errors.Is(err, fs.ErrNotExist)), or the file is there and unusable in some
// way (errors.Is(err, helpers.ErrLockfileInvalid) is true - unreadable,
// unparseable, or internally inconsistent). Three consumers depend on that
// dichotomy being exhaustive: resolveOrLoadLockfile and lockFrozen both
// classify --frozen's behavior by which arm holds, and lockDryRunBaseline
// treats anything that is not IsNotExist the same way (warn, then proceed as
// if no baseline existed) - none of the three have a third case to fall
// into.
func Load(path string) (*File, error) {
	//nolint:gosec // path is user-provided lockfile location.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// Wrapped with %s, not %w: the fs.ErrNotExist guard above already
		// makes this arm and the one above mutually exclusive, so %w buys no
		// additional exclusivity here. %s is chosen instead because it
		// matches the sibling YAML-unmarshal arm two lines below, and
		// because nothing in this program branches on the underlying errno,
		// so keeping it reachable through errors.Is would buy nothing.
		// err.Error() still carries the real OS error text for a human
		// reading the message; it is simply not reachable through errors.Is.
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

// marshal renders f as the one byte sequence both Save and Hash use. Sharing
// it is what keeps Hash the SHA256 of the file Save wrote, so the indent is
// part of the contract `go-galaxy hash` prints: changing it changes every
// CI cache key built on a lockfile.
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

// validate rejects lockfiles that are internally inconsistent, on two
// independent grounds: a duplicate collection name, which would otherwise
// let indexLockfile silently drop an entry and make a frozen install
// ambiguous, and an entry whose pinned version is not helpers.IsExactVersion
// - a constraint string like "*" rather than a version anything can install.
// The latter closes the route a --frozen install would otherwise take when a
// lockfile entry carried an unresolved constraint: resolveFromLockfile would
// build a collection from it, exactVersionFromConstraints would treat it as
// unpinned, and the run would silently install the server's highest version
// under a path built from the literal constraint text. Save does not call
// this, and the two shapes it would have caught are ruled out at the
// producer by two different mechanisms, not one: buildLockfile carries its
// own helpers.IsExactVersion guard over the resolved map it builds a File
// from, so it cannot hand Save a non-exact version; a duplicate name simply
// cannot arise in the first place, because that same map is keyed by fqdn,
// so two entries can never share a name to begin with.
//
// A git entry is judged on top of that by validateGitEntry, and a schema-1
// file carrying one is refused outright: the schema is what tells an older
// binary to stop, so a file that claims the old schema while carrying the
// new shape has been edited by hand.
func (f *File) validate() error {
	seen := make(map[string]struct{}, len(f.Collections))
	for _, e := range f.Collections {
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate collection name %q", helpers.ErrLockfileInvalid, e.Name)
		}
		seen[e.Name] = struct{}{}
		// Checked before the version, and quoted, because until it passes
		// nothing here knows what e.Name contains: the version message below
		// prints the name, and a name carrying a newline would compose extra
		// lines into the very error reporting it. Once this check has passed,
		// a name is an ordinary identifier again.
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

// checkEntryName judges an entry's name by the alphabet its type earns: a
// url entry's identity came from its artifact's own MANIFEST.json and is
// held to the relaxed rule that admits it (see
// helpers.IsURLCollectionNamePart), every other entry to the Galaxy
// alphabet a server-resolved name has already passed.
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

// validateEntryType judges the fields that distinguish a git or url entry
// from a Galaxy one. A Galaxy entry may carry none of them; a git entry must
// carry a canonical repository URL as its source, a valid ref, a full
// lowercase commit and a valid subdir, must not carry a SHA256 (see Entry for
// why), and may only appear in a schema-2 or later file; a url entry must
// carry a canonical tarball URL as its source and a full lowercase sha256,
// nothing of the git triple, and may only appear in a schema-4 file. The URL
// is re-parsed rather than trusted because the source is repository content:
// anything a later run connects to has to pass the same grammar a
// requirements entry does.
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

// sourceHasUserinfo reports whether an entry's source embeds URL userinfo
// ("https://user:pass@hub/"). It is the lockfile's half of a rule
// requirements.checkSourceUserinfo already applies to the same value arriving
// through the other boundary it enters by, and it exists because a lockfile is
// repository content just as requirements.yml is: an entry's source flows
// unchanged into root-metadata request URLs, warning lines, the resolved
// snapshot, and GALAXY.yml, and url.URL.String() renders a userinfo password
// back out in plain text at every one of them. It also decides what
// credential goes to that host at all - net/http sets Basic auth from a URL's
// userinfo before any transport runs, and internal/galaxy/fetch's
// authTransport declines to attach the operator's configured token to a
// request that already carries an Authorization header - so a source with
// userinfo substitutes the repository's credential for the operator's.
//
// A source naming a bare server_list id (e.g. "internal", never URL-shaped)
// is left alone, the same exception its sibling documents: url.Parse succeeds
// on such a value but yields no scheme and no host, so the userinfo branch is
// unreachable for it.
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

// LoadRequired is Load for a caller that cannot proceed without the lockfile:
// an absent file becomes helpers.ErrLockfileMissing naming the path, instead
// of the bare fs.ErrNotExist Load returns.
//
// The distinction it draws is between commands, not between failures. "The
// lockfile you asked me to read is not there" is a fact about the lockfile,
// which is why it classifies as the lockfile exit class; a bare fs.ErrNotExist
// reaching a command instead lands it in the environment-usage class, the
// wrong class for that fact. Every command that requires a lockfile goes
// through here, so all of them stay in the lockfile class and the
// classification is a property of the loader rather than something each call
// site has to remember.
//
// hash is the one deliberate exception and does not call this: a missing
// lockfile there is not a failure at all, since it falls back to hashing the
// requirements file for repositories that do not lock. It stays on Load and
// branches on IsNotExist itself.
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

// canonicalize sorts the entries and their deps and sets the schema version
// from the entries, unconditionally: the schema is a function of the
// content, never a value a producer chooses, which is what keeps a Galaxy-only
// file at schema 1 and flips a file to schema 2 exactly when its first git
// entry appears.
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

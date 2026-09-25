package collections

import (
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// collection is a resolved collection. Source is the owning server base, or
// the git or url locator every consumer keys on, so a new commit or content
// reads as a source change; Ref is the requested git ref, for the lockfile.
type collection struct {
	Namespace  string `yaml:"namespace"`
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Source     string `yaml:"source"`
	Constraint string `yaml:"-"`
	Type       string `yaml:"-"`
	Ref        string `yaml:"-"`
	// SHA256 is the artifact pin the install enforces byte for byte: the
	// frozen-lockfile pin, or a url collection's locator digest; never serialized.
	SHA256 string `yaml:"-"`
	// DownloadURL is a frozen Galaxy entry's locked download_url, fetched in
	// place of the version metadata; empty when this run verifies signatures.
	DownloadURL string   `yaml:"-"`
	Signatures  []string `yaml:"signatures"`
}

const (
	typeGalaxy = "galaxy"
	typeGit    = "git"
	typeURL    = "url"
)

// fqdn returns the collection's namespace.name.
func (c collection) fqdn() string {
	return c.Namespace + "." + c.Name
}

// key returns the unique key for the collection.
func (c collection) key() string {
	return fmt.Sprintf("%s.%s@%s", c.Namespace, c.Name, c.Version)
}

// isGit reports whether the collection comes from a git source. It reads the
// Source prefix, not Type, because lockfile and snapshot entries carry the
// locator but not Type.
func (c collection) isGit() bool {
	return gitsource.IsLocator(c.Source)
}

// gitLocator parses the collection's locator. It is only meaningful when
// isGit reports true.
func (c collection) gitLocator() (gitsource.Locator, error) {
	return gitsource.ParseLocator(c.Source)
}

// isURL reports whether the collection comes from a url source, by the same
// Source-prefix dispatch isGit uses and for the same reason.
func (c collection) isURL() bool {
	return urlsource.IsLocator(c.Source)
}

// urlLocator parses the collection's locator. It is only meaningful when
// isURL reports true.
func (c collection) urlLocator() (urlsource.Locator, error) {
	return urlsource.ParseLocator(c.Source)
}

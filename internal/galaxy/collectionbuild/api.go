package collectionbuild

import (
	"context"
	"os"
)

// GalaxyYML is the build metadata of one collection, read from its galaxy.yml
// or, for a source directory that ships a built tree, from its MANIFEST.json
// collection_info. Field names follow the galaxy.yml keys.
type GalaxyYML struct {
	Dependencies  map[string]string
	Namespace     string
	Name          string
	Version       string
	Readme        string
	Description   string
	LicenseFile   string
	Repository    string
	Documentation string
	Homepage      string
	Issues        string
	Authors       []string
	License       []string
	Tags          []string
	BuildIgnore   []string
}

// Candidate is one collection directory Discover found. FromManifest marks
// metadata read from MANIFEST.json rather than galaxy.yml; the tree is then
// rebuilt from scratch, as ansible-galaxy does for an installed tree.
type Candidate struct {
	Subdir       string
	Meta         GalaxyYML
	FromManifest bool
}

// Built is one artifact Build produced: identity and raw dependencies from
// its metadata, the temp tar.gz path and its sha256, the build warnings, and
// a Cleanup that removes the temp file.
type Built struct {
	Cleanup      func()
	Dependencies map[string]string
	Namespace    string
	Name         string
	Version      string
	Subdir       string
	ArtifactPath string
	SHA256       string
	Warnings     []string
}

// TempFileFunc hands Build the file it writes the artifact into; the cleanup
// removes it. See gitsource.TempFileFunc for why the caller supplies it.
type TempFileFunc func(ctx context.Context) (*os.File, func(), error)

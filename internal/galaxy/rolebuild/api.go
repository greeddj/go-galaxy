package rolebuild

import (
	"context"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
)

// Meta is what a role's metadata tells the installer: meta/main.yml's
// dependencies with meta/requirements.yml's appended, each as written; the
// galaxy_info.role_name ("" when absent); and the warnings the parse raised.
type Meta struct {
	Dependencies []gitsource.RoleDependency
	RoleName     string
	Warnings     []string
}

// Built is one artifact Build produced: its metadata, temp path and sha256,
// the warnings raised (the walk's first, then Meta's), and an idempotent
// Cleanup that removes the temp file.
type Built struct {
	Cleanup      func()
	Meta         Meta
	ArtifactPath string
	SHA256       string
	Warnings     []string
}

// TempFileFunc hands Build the file it writes the artifact into; the cleanup
// removes it. See gitsource.TempFileFunc for why the caller supplies it.
type TempFileFunc func(ctx context.Context) (*os.File, func(), error)

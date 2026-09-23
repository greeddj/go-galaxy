// Package gitsource is the git source grammar (URL, ref, subdir, locator,
// credential matching) and the Client seam; it imports no go-git, so the
// requirements, lockfile, store and cleanup layers never link a transport.
package gitsource

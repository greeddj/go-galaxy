// Package gitsource is the git source grammar (URL, ref, subdir, locator,
// credential matching), the Client seam and its --offline refusal. It imports
// no go-git, so requirements, lockfile, store and cleanup never link a transport.
package gitsource

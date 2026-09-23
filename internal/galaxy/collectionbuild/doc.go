// Package collectionbuild builds a collection tar.gz from a treearchive.Source
// the way ansible-galaxy collection build would, refusing by name what it will
// not reproduce. It opens no filesystem path and self-checks every artifact.
package collectionbuild

// Package rolebuild turns a role's source tree into a deterministic tar.gz
// with no lead documents and every entry at the top level. It reads the tree
// only through treearchive.Source and carries dependency specs unjudged.
package rolebuild

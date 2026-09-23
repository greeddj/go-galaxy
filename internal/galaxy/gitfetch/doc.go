// Package gitfetch is the only production importer of go-git: it implements
// gitsource.Client by advertising once, fetching by hash into a byte-capped
// on-disk store and reading the commit's tree without any checkout.
package gitfetch

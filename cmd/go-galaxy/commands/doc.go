// Package commands defines the urfave/cli command tree and is wiring only: the
// work lives under internal/galaxy. install, cleanup, lock, warm and outdated
// share runCollectionCommand; hash, tree and explain read files on disk only.
package commands

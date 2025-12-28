package local

import "errors"

var (
	errCacheDirEmpty    = errors.New("cache directory is empty")
	errArtifactKeyEmpty = errors.New("artifact key is empty")
)

const (
	dirMod = 0o755
)

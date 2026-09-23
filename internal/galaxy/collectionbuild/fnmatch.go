package collectionbuild

import "github.com/greeddj/go-galaxy/internal/galaxy/treearchive"

// defaultPatternCount is how many patterns newIgnoreRules puts ahead of
// build_ignore.
const defaultPatternCount = 9

// ignoreDirNames are the directory basenames ansible prunes at every depth.
func ignoreDirNames() map[string]struct{} {
	return map[string]struct{}{
		"CVS": {}, ".bzr": {}, ".hg": {}, ".git": {}, ".svn": {}, "__pycache__": {}, ".tox": {},
	}
}

// newIgnoreRules is ansible's default exclusions, then build_ignore in order,
// each fnmatched against the path relative to the collection root, so
// "tests/output" matches at the root only; DirNames prunes at every depth.
func newIgnoreRules(namespace, name string, buildIgnore []string) treearchive.Rules {
	patterns := make([]string, 0, defaultPatternCount+len(buildIgnore))
	patterns = append(patterns,
		"MANIFEST.json",
		"FILES.json",
		"galaxy.yml",
		"galaxy.yaml",
		".git",
		"*.pyc",
		"*.retry",
		"tests/output",
		namespace+"-"+name+"-*.tar.gz",
	)
	patterns = append(patterns, buildIgnore...)
	return treearchive.Rules{Patterns: patterns, DirNames: ignoreDirNames()}
}

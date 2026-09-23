package proseaudit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// maxCommentLines is the longest comment block any committed file may carry;
// reasoning that needs more belongs in docs/, not beside the code.
const maxCommentLines = 3

// directiveLine matches a comment line a tool reads rather than a reader:
// `//go:build`, `//nolint:...` and gosec's `// #nosec`. Those are not counted.
var directiveLine = regexp.MustCompile(`^//([a-z0-9]+:|\s*#nosec)`)

// TestCommentBlocksAreShort is the gate: no comment block in a tracked Go file,
// shell script, YAML file, Justfile, Dockerfile or .gitignore runs past
// maxCommentLines, directive lines aside.
func TestCommentBlocksAreShort(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	fset := token.NewFileSet()

	var problems []string
	for _, rel := range trackedFiles(t, root) {
		if !strings.HasSuffix(rel, ".go") && !isHashCommented(rel) {
			continue
		}
		// #nosec G304 -- rel comes from git's own listing of this repository
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			t.Fatalf("reading %s: %v", rel, err)
		}
		hits, err := longCommentHits(fset, rel, data)
		if err != nil {
			t.Fatalf("scanning %s: %v", rel, err)
		}
		problems = append(problems, hits...)
	}

	if len(problems) > 0 {
		t.Fatalf("comment blocks over %d lines (move the reasoning to docs/):\n\t%s",
			maxCommentLines, strings.Join(problems, "\n\t"))
	}
}

// TestLongCommentHitsCountsTheLimit pins the scanner against fixtures on both
// sides of the limit, for Go and for a `#`-commented file alike.
func TestLongCommentHitsCountsTheLimit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		file     string
		source   string
		wantHits int
	}{
		{
			name:   "go_three_lines",
			file:   "fixture.go",
			source: "package fixture\n\n// One.\n// Two.\n// Three.\nfunc F() {}\n",
		},
		{
			name:     "go_four_lines",
			file:     "fixture.go",
			source:   "package fixture\n\n// One.\n//\n// Three.\n// Four.\nfunc F() {}\n",
			wantHits: 1,
		},
		{
			name:   "go_directives_not_counted",
			file:   "fixture.go",
			source: "package fixture\n\n// One.\n// Two.\n// Three.\n//nolint:gochecknoglobals // a table\nvar V = 1\n",
		},
		{
			name:     "go_block_comment",
			file:     "fixture.go",
			source:   "package fixture\n\n/*\nOne.\nTwo.\n*/\nfunc F() {}\n",
			wantHits: 1,
		},
		{
			name:   "hash_three_lines_after_shebang",
			file:   "fixture.sh",
			source: "#!/bin/sh\n# One.\n# Two.\n# Three.\necho\n",
		},
		{
			name:     "hash_four_lines",
			file:     "fixture.yml",
			source:   "a: 1\n  # One.\n  # Two.\n  # Three.\n  # Four.\nb: 2\n",
			wantHits: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hits, err := longCommentHits(token.NewFileSet(), tc.file, []byte(tc.source))
			if err != nil {
				t.Fatalf("longCommentHits: %v", err)
			}
			if len(hits) != tc.wantHits {
				t.Fatalf("got %d hits %q, want %d", len(hits), hits, tc.wantHits)
			}
		})
	}
}

// longCommentHits reports every comment block in data longer than
// maxCommentLines, as `name:line: N lines`.
func longCommentHits(fset *token.FileSet, name string, data []byte) ([]string, error) {
	if strings.HasSuffix(name, ".go") {
		return longGoComments(fset, name, data)
	}
	return longHashComments(name, data)
}

// longGoComments counts each comment group by the lines it spans, less the
// directive lines inside it.
func longGoComments(fset *token.FileSet, name string, data []byte) ([]string, error) {
	file, err := parser.ParseFile(fset, name, data, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var hits []string
	for _, group := range file.Comments {
		first := fset.Position(group.Pos()).Line
		lines := fset.Position(group.End()).Line - first + 1
		for _, comment := range group.List {
			if directiveLine.MatchString(comment.Text) {
				lines--
			}
		}
		if lines > maxCommentLines {
			hits = append(hits, fmt.Sprintf("%s:%d: %d lines", name, first, lines))
		}
	}
	return hits, nil
}

// longHashComments counts runs of consecutive `#` lines; a shebang on the
// first line is not a comment.
func longHashComments(name string, data []byte) ([]string, error) {
	var hits []string
	start, run, line := 0, 0, 0
	flush := func() {
		if run > maxCommentLines {
			hits = append(hits, fmt.Sprintf("%s:%d: %d lines", name, start, run))
		}
		run = 0
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(text, "#") || (line == 1 && strings.HasPrefix(text, "#!")) {
			flush()
			continue
		}
		if run == 0 {
			start = line
		}
		run++
	}
	flush()
	return hits, scanner.Err()
}

// isHashCommented reports whether rel is a file type whose comments start
// with `#` and fall under the gate.
func isHashCommented(rel string) bool {
	switch filepath.Base(rel) {
	case "Justfile", "Dockerfile", ".gitignore":
		return true
	}
	switch filepath.Ext(rel) {
	case ".sh", ".yml", ".yaml":
		return true
	}
	return false
}

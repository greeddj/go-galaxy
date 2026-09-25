package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// writeGalaxyEntryFile writes a one-entry lockfile at schema whose Galaxy
// collection carries downloadURL, omitting the key when it is empty.
func writeGalaxyEntryFile(t *testing.T, schema int, downloadURL string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("collections:\n  - name: acme.widgets\n    version: 1.0.0\n    source: https://galaxy.example.invalid\n")
	if downloadURL != "" {
		fmt.Fprintf(&b, "    download_url: %q\n", downloadURL)
	}
	fmt.Fprintf(&b, "schema_version: %d\n", schema)
	path := filepath.Join(t.TempDir(), DefaultName)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
	return path
}

// TestLoadRefusesAGalaxyEntryBelowSchemaFive pins that every older schema
// holding a Galaxy entry is refused with the command that rewrites it, even
// when the entry already names a download_url; schema 5 is the control.
func TestLoadRefusesAGalaxyEntryBelowSchemaFive(t *testing.T) {
	t.Parallel()
	good := downloadURLFor("acme.widgets", "1.0.0")
	for schema := SchemaVersion; schema < SchemaVersionDownloadURL; schema++ {
		_, err := Load(writeGalaxyEntryFile(t, schema, good))
		if !errors.Is(err, helpers.ErrLockfileInvalid) {
			t.Fatalf("schema %d: Load = %v, want errors.Is helpers.ErrLockfileInvalid", schema, err)
		}
		if !strings.Contains(err.Error(), "go-galaxy lock") {
			t.Fatalf("schema %d: refusal %q does not name the command that rewrites the file", schema, err)
		}
	}
	f, err := Load(writeGalaxyEntryFile(t, SchemaVersionDownloadURL, good))
	if err != nil {
		t.Fatalf("schema %d: Load = %v, want nil", SchemaVersionDownloadURL, err)
	}
	if got := f.Collections[0].DownloadURL; got != good {
		t.Fatalf("DownloadURL = %q, want %q", got, good)
	}
}

// malformedDownloadURLCase is one row of TestLoadRefusesAMalformedDownloadURL:
// the download_url written and the reason its refusal must name.
type malformedDownloadURLCase struct {
	name   string
	url    string
	reason string
}

// malformedDownloadURLCases gives every check in downloadURLProblem a row of
// its own, so deleting one fails on the reason it owns.
func malformedDownloadURLCases() []malformedDownloadURLCase {
	const notHTTP = "not an absolute http(s) URL"
	return []malformedDownloadURLCase{
		{name: "missing", url: "", reason: "requires a download_url"},
		{name: "relative", url: "/download/acme-widgets-1.0.0.tar.gz", reason: notHTTP},
		{name: "ftp scheme", url: "ftp://h.example.invalid/acme-widgets-1.0.0.tar.gz", reason: notHTTP},
		{name: "no host", url: "https:///acme-widgets-1.0.0.tar.gz", reason: notHTTP},
		{name: "unparseable escape", url: "https://h.example.invalid/%zz", reason: notHTTP},
		// #nosec G101 -- test fixture literal, not a real credential
		{name: "userinfo", url: "https://user:hunter2@h.example.invalid/a.tar.gz", reason: "must not carry userinfo"},
		{name: "presigned query", url: "https://h.example.invalid/a.tar.gz?X-Amz-Signature=abc", reason: "must not carry a query"},
		{name: "empty query", url: "https://h.example.invalid/a.tar.gz?", reason: "must not carry a query"},
		{name: "fragment", url: "https://h.example.invalid/a.tar.gz#part", reason: "must not carry a fragment"},
		{name: "upper-case scheme", url: "HTTPS://h.example.invalid/a.tar.gz", reason: "not a canonical URL"},
	}
}

// TestLoadRefusesAMalformedDownloadURL pins each download_url refusal and
// that no refusal echoes the URL, since a refused one may carry a password;
// TestLoadRefusesAGalaxyEntryBelowSchemaFive holds the loading control.
func TestLoadRefusesAMalformedDownloadURL(t *testing.T) {
	t.Parallel()
	for _, tc := range malformedDownloadURLCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeGalaxyEntryFile(t, SchemaVersionDownloadURL, tc.url))
			if !errors.Is(err, helpers.ErrLockfileInvalid) {
				t.Fatalf("Load = %v, want errors.Is helpers.ErrLockfileInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Load = %v, want the reason %q", err, tc.reason)
			}
			if tc.url != "" && strings.Contains(err.Error(), tc.url) {
				t.Fatalf("Load error echoes the download_url: %v", err)
			}
		})
	}
}

// TestLoadRefusesADownloadURLOnAGitOrURLEntry pins that download_url belongs
// to a Galaxy entry alone: a git or url entry fetches from its own source.
func TestLoadRefusesADownloadURLOnAGitOrURLEntry(t *testing.T) {
	t.Parallel()
	for name, e := range map[string]Entry{"git": gitEntry(), "url": urlEntry()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e.DownloadURL = downloadURLFor(e.Name, e.Version)
			path := filepath.Join(t.TempDir(), DefaultName)
			if err := Save(path, &File{Collections: []Entry{e}}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			_, err := Load(path)
			if !errors.Is(err, helpers.ErrLockfileInvalid) || !strings.Contains(err.Error(), "belongs to a Galaxy entry") {
				t.Fatalf("Load = %v, want a download_url refusal wrapping helpers.ErrLockfileInvalid", err)
			}
		})
	}
}

// TestCompareAndHashCoverTheDownloadURL pins that a moved download_url alone
// is drift: Compare reports it as its own field and Hash changes with it.
func TestCompareAndHashCoverTheDownloadURL(t *testing.T) {
	t.Parallel()
	entry := Entry{Name: "acme.widgets", Version: "1.0.0", DownloadURL: downloadURLFor("acme.widgets", "1.0.0")}
	moved := entry
	moved.DownloadURL = "https://cdn.example.invalid/acme-widgets-1.0.0.tar.gz"
	before, after := &File{Collections: []Entry{entry}}, &File{Collections: []Entry{moved}}

	diff := Compare(before, after)
	if len(diff.Updated) != 1 {
		t.Fatalf("Updated = %+v, want the one moved entry", diff.Updated)
	}
	fields := diff.Updated[0].Fields()
	want := FieldChange{Field: "download_url", From: entry.DownloadURL, To: moved.DownloadURL}
	if len(fields) != 1 || fields[0] != want {
		t.Fatalf("Fields = %+v, want [%+v]", fields, want)
	}

	beforeHash, err := before.Hash()
	if err != nil {
		t.Fatal(err)
	}
	afterHash, err := after.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash == afterHash {
		t.Fatal("Hash did not change with the download_url")
	}
}

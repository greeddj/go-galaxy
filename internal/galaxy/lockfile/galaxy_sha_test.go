package lockfile

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestLoadHoldsAGalaxySHA256ToItsShape pins that a Galaxy entry's sha256 is
// empty, a digest-less server's pin, or 64 lowercase hex digits; anything else
// is refused at load rather than surfacing as an install mismatch.
func TestLoadHoldsAGalaxySHA256ToItsShape(t *testing.T) {
	t.Parallel()
	good := strings.Repeat("ab", 32)
	cases := map[string]struct {
		sha string
		ok  bool
	}{
		"empty":         {sha: "", ok: true},
		"lowercase hex": {sha: good, ok: true},
		"uppercase hex": {sha: strings.ToUpper(good), ok: false},
		"short":         {sha: "abc123", ok: false},
		"not hex":       {sha: strings.Repeat("zz", 32), ok: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), DefaultName)
			e := Entry{
				Name: "acme.widgets", Version: "1.0.0", Source: "https://galaxy.example.invalid",
				DownloadURL: downloadURLFor("acme.widgets", "1.0.0"), SHA256: tc.sha,
			}
			if err := Save(path, &File{Collections: []Entry{e}}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			_, err := Load(path)
			if tc.ok && err != nil {
				t.Fatalf("Load = %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, helpers.ErrLockfileInvalid) {
				t.Fatalf("Load = %v, want errors.Is helpers.ErrLockfileInvalid", err)
			}
		})
	}
}

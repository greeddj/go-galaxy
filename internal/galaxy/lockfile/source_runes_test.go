package lockfile

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// forgedLine is a source carrying a line that reads like lock's own success
// line, the shape a crafted galaxy.lock would print through a diff line.
const forgedLine = "https://evil.example/\n✔ Lockfile written to galaxy.lock (0 collections, 1 role)"

// sourceRunesGalaxyEntry is a loadable Galaxy collection entry from source.
func sourceRunesGalaxyEntry(source string) Entry {
	return Entry{
		Name: "acme.widgets", Version: "1.0.0", Source: source,
		DownloadURL: "https://galaxy.example/download/acme-widgets-1.0.0.tar.gz",
	}
}

// sourceRunesGalaxyRole is a loadable Galaxy role entry from source.
func sourceRunesGalaxyRole(source string) RoleEntry {
	return RoleEntry{
		Name: "geerlingguy.docker", Type: RoleTypeGalaxy, Version: "8.0.0", Galaxy: "geerlingguy.docker", Source: source,
		Repository: "https://github.com/geerlingguy/ansible-role-docker", Ref: "refs/tags/8.0.0", Commit: strings.Repeat("a", 40),
	}
}

// TestLoadRefusesAControlCharacterInAPrintedSource pins that a source lock
// prints as written, the top-level server and a Galaxy collection's or role's
// source, may carry no control character, so no line of output can be forged.
func TestLoadRefusesAControlCharacterInAPrintedSource(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		file   File
		refuse bool
	}{
		"server with a line break":        {file: File{Server: forgedLine}, refuse: true},
		"Galaxy source with a line break": {file: File{Collections: []Entry{sourceRunesGalaxyEntry(forgedLine)}}, refuse: true},
		"Galaxy source with an escape": {
			file: File{Collections: []Entry{sourceRunesGalaxyEntry("https://galaxy.example/\x1b[2K")}}, refuse: true,
		},
		"Galaxy role source with a line break": {file: File{Roles: []RoleEntry{sourceRunesGalaxyRole(forgedLine)}}, refuse: true},
		"Galaxy role source of a server id":    {file: File{Roles: []RoleEntry{sourceRunesGalaxyRole("galaxy")}}},
		"Galaxy source and server URLs": {file: File{
			Server: "https://galaxy.example", Collections: []Entry{sourceRunesGalaxyEntry("https://galaxy.example")},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "galaxy.lock")
			if err := Save(path, &tc.file); err != nil {
				t.Fatalf("Save: %v", err)
			}
			_, err := Load(path)
			if !tc.refuse {
				if err != nil {
					t.Fatalf("Load = %v, want the file to load", err)
				}
				return
			}
			if !errors.Is(err, helpers.ErrLockfileInvalid) {
				t.Fatalf("Load = %v, want errors.Is helpers.ErrLockfileInvalid", err)
			}
			if strings.Contains(err.Error(), "evil.example") || strings.ContainsAny(err.Error(), "\n\x1b") {
				t.Fatalf("Load error %q prints the refused source", err)
			}
		})
	}
}

package archive

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestSanitizeArchivePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{name: "empty", input: "", wantErr: helpers.ErrArchiveEntryHasEmptyName},
		{name: "abs", input: "/etc/passwd", wantErr: helpers.ErrArchiveEntryIsAbsolutePath},
		{name: "escape", input: "../evil", wantErr: helpers.ErrArchiveEntryEscapesDestination},
		{name: "dot", input: ".", want: ""},
		{name: "ok", input: "dir/file", want: filepath.FromSlash("dir/file")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sanitizeArchivePath(tt.input)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

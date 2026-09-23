package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

// TestFormatVersion exercises all four format branches of formatVersion with
// explicit, already-resolved (ldflags-style) inputs, so the expected output
// is deterministic regardless of the environment's build info.
func TestFormatVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		commit  string
		date    string
		builtBy string
		want    string
		name    string
	}{
		{
			name:    "commit and date both set",
			version: "v1.2.3",
			commit:  "abc123",
			date:    "2026-07-22T00:00:00Z",
			builtBy: "goreleaser",
			want:    "v1.2.3 (commit abc123, built by goreleaser @ 2026-07-22T00:00:00Z) // " + runtime.Version(),
		},
		{
			name:    "commit set, date empty",
			version: "v1.2.3",
			commit:  "abc123",
			date:    "",
			builtBy: "goreleaser",
			want:    "v1.2.3 (commit abc123, built by goreleaser) // " + runtime.Version(),
		},
		{
			name:    "commit empty, date set",
			version: "v1.2.3",
			commit:  "",
			date:    "2026-07-22T00:00:00Z",
			builtBy: "goreleaser",
			want:    "v1.2.3 (built by goreleaser @ 2026-07-22T00:00:00Z) // " + runtime.Version(),
		},
		{
			name:    "commit and date both empty",
			version: "v1.2.3",
			commit:  "",
			date:    "",
			builtBy: "goreleaser",
			want:    "v1.2.3 (built by goreleaser) // " + runtime.Version(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatVersion(tt.version, tt.commit, tt.date, tt.builtBy)
			if got != tt.want {
				t.Errorf("formatVersion(%q, %q, %q, %q) = %q, want %q",
					tt.version, tt.commit, tt.date, tt.builtBy, got, tt.want)
			}
		})
	}
}

// TestVersion checks the public entry point with full, explicit ldflags-style
// inputs (the real-world Justfile case), which must pass through unchanged
// and produce a deterministic string.
func TestVersion(t *testing.T) {
	t.Parallel()
	got := Version("v1.2.3", "abc123", "2026-07-22T00:00:00Z", "goreleaser")
	want := "v1.2.3 (commit abc123, built by goreleaser @ 2026-07-22T00:00:00Z) // " + runtime.Version()
	if got != want {
		t.Errorf("Version() = %q, want %q", got, want)
	}
}

// TestVersionDevBuildFallback pins the ldflags-less path, whose exact output
// depends on how the test binary was built: never empty, still names the Go
// runtime, and carries no URL, since no network fetch may be made for it.
func TestVersionDevBuildFallback(t *testing.T) {
	t.Parallel()
	got := Version("", "", "", "")
	if got == "" {
		t.Fatal("Version(\"\", \"\", \"\", \"\") = \"\", want non-empty")
	}
	if !strings.Contains(got, runtime.Version()) {
		t.Errorf("Version() = %q, want it to contain runtime.Version() = %q", got, runtime.Version())
	}
	if strings.Contains(got, "http") {
		t.Errorf("Version() = %q, must not contain \"http\" (no network fetch should remain)", got)
	}
}

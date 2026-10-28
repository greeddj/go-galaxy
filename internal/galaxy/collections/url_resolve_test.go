package collections

import "testing"

type resolveURLCase struct {
	name string
	base string
	ref  string
	want string
}

// resolveURLCases covers every arm of resolveURL: an empty or unparsable
// side, an absolute reference, and each way a rooted path meets a base path.
func resolveURLCases() []resolveURLCase {
	return []resolveURLCase{
		{
			name: "empty base returns the trimmed reference",
			base: "  ",
			ref:  " /api/v3/collections/acme/app/versions/ ",
			want: "/api/v3/collections/acme/app/versions/",
		},
		{
			name: "unparsable base is joined with one slash",
			base: "http://[::1",
			ref:  "/versions/",
			want: "http://[::1/versions/",
		},
		{
			name: "unparsable reference is joined with one slash",
			base: "https://hub.example/api/",
			ref:  ":versions",
			want: "https://hub.example/api/:versions",
		},
		{
			name: "absolute reference wins over the base",
			base: "https://hub.example/galaxy",
			ref:  "https://mirror.example/api/v3/collections/acme/app/versions/",
			want: "https://mirror.example/api/v3/collections/acme/app/versions/",
		},
		{
			name: "api path is merged under the base path prefix",
			base: "https://hub.example/galaxy/",
			ref:  "/api/v3/collections/acme/app/versions/?page=2#top",
			want: "https://hub.example/galaxy/api/v3/collections/acme/app/versions/?page=2#top",
		},
		{
			name: "path already under the base path is not merged twice",
			base: "https://hub.example/api/galaxy",
			ref:  "/api/galaxy/v3/collections/acme/app/versions/",
			want: "https://hub.example/api/galaxy/v3/collections/acme/app/versions/",
		},
		{name: "path equal to the base path stays as written", base: "https://hub.example/api", ref: "/api", want: "https://hub.example/api"},
		{
			name: "root path outside the api tree replaces the base path",
			base: "https://hub.example/galaxy",
			ref:  "/download/acme-app-1.0.0.tar.gz",
			want: "https://hub.example/download/acme-app-1.0.0.tar.gz",
		},
		{
			name: "base without a path takes the reference path",
			base: "https://hub.example",
			ref:  "/api/v3/collections/acme/app/versions/",
			want: "https://hub.example/api/v3/collections/acme/app/versions/",
		},
		{
			name: "relative reference resolves under the base directory",
			base: "https://hub.example/api/v3/",
			ref:  "collections/acme/app/versions/",
			want: "https://hub.example/api/v3/collections/acme/app/versions/",
		},
	}
}

// TestResolveURLAgainstServerBase pins how a server-chosen reference resolves
// against its source: an absolute one wins, an /api/ path merges under the
// base path.
func TestResolveURLAgainstServerBase(t *testing.T) {
	t.Parallel()
	for _, tt := range resolveURLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveURL(tt.base, tt.ref); got != tt.want {
				t.Fatalf("resolveURL(%q, %q) = %q, want %q", tt.base, tt.ref, got, tt.want)
			}
		})
	}
}

// TestJoinURLAndBasePathMergeEdges pins the two helpers' empty-side answers,
// which resolveURL never reaches with a non-empty base but which must not
// produce a doubled or leading slash.
func TestJoinURLAndBasePathMergeEdges(t *testing.T) {
	t.Parallel()

	joins := []struct {
		base string
		ref  string
		want string
	}{
		{base: "", ref: "/versions/", want: "versions/"},
		{base: "https://hub.example/", ref: "", want: "https://hub.example"},
		{base: "https://hub.example//", ref: "//versions/", want: "https://hub.example/versions/"},
	}
	for _, tt := range joins {
		if got := joinURL(tt.base, tt.ref); got != tt.want {
			t.Errorf("joinURL(%q, %q) = %q, want %q", tt.base, tt.ref, got, tt.want)
		}
	}

	merges := []struct {
		basePath string
		refPath  string
		want     bool
	}{
		{basePath: "", refPath: "/api/v3/", want: false},
		{basePath: "/galaxy", refPath: "", want: false},
		{basePath: "/galaxy", refPath: "/galaxy/api/", want: false},
		{basePath: "/galaxy", refPath: "/api/v3/", want: true},
		{basePath: "/galaxy", refPath: "/content/", want: false},
	}
	for _, tt := range merges {
		if got := needsBasePathMerge(tt.basePath, tt.refPath); got != tt.want {
			t.Errorf("needsBasePathMerge(%q, %q) = %v, want %v", tt.basePath, tt.refPath, got, tt.want)
		}
	}
}

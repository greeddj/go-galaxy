package requirements

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const urlTestTarball = "https://github.com/acme/kafka/releases/download/0.24.0/acme-kafka-0.24.0.tar.gz"

// TestParseCollectionsAcceptsURLShapes pins the spellings a url source may
// take: the bare string ansible infers a url source from, the explicit
// type: url mapping, and an exact version assertion beside either.
type urlShapeCase struct {
	input       string
	wantVersion string
}

func urlShapeCases() map[string]urlShapeCase {
	return map[string]urlShapeCase{
		"bare string":         {input: "- " + urlTestTarball + "\n"},
		"name only":           {input: "- name: " + urlTestTarball + "\n"},
		"explicit type":       {input: "- name: " + urlTestTarball + "\n  type: url\n"},
		"exact version":       {input: "- name: " + urlTestTarball + "\n  version: 0.24.0\n", wantVersion: "0.24.0"},
		"star version":        {input: "- name: " + urlTestTarball + "\n  version: \"*\"\n"},
		"query kept":          {input: "- name: " + urlTestTarball + "?token=t\n"},
		"scheme folds":        {input: "- HTTPS://github.com/acme/x.tar.gz\n"},
		"non-tarball suffix":  {input: "- https://dl.example/artifacts/kafka/latest\n"},
		"loopback plain http": {input: "- http://127.0.0.1:8080/x.tar.gz\n"},
		"caching-proxy path":  {input: "- http://cacheproxy.mirror.example.com/" + urlTestTarball + "\n"},
	}
}

func TestParseCollectionsAcceptsURLShapes(t *testing.T) {
	t.Parallel()
	for name, tc := range urlShapeCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, err := Parse([]byte("collections:\n"+tc.input), "https://default")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(file.Collections) != 1 {
				t.Fatalf("collections = %+v, want one entry", file.Collections)
			}
			assertURLRequirement(t, file.Collections[0], tc.wantVersion)
		})
	}
}

// assertURLRequirement checks the one shape every accepted url entry takes:
// type url, no identity, no git fields, a canonical http(s) Source, and the
// version the case expects.
func assertURLRequirement(t *testing.T, req CollectionRequirement, wantVersion string) {
	t.Helper()
	if !req.IsURL() || req.Namespace != "" || req.Name != "" || req.Ref != "" || req.Subdir != "" {
		t.Fatalf("url requirement shape = %+v", req)
	}
	if req.Version != wantVersion {
		t.Fatalf("Version = %q, want %q", req.Version, wantVersion)
	}
	if req.Source == "" || req.Source[:4] != "http" {
		t.Fatalf("Source = %q, want the canonical URL", req.Source)
	}
}

// TestParseCollectionsRejectsURLShapes walks the refusals a url entry can
// earn at load, before any request is made.
func TestParseCollectionsRejectsURLShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		wantErr error
		input   string
	}{
		"signatures key": {
			input:   "- name: " + urlTestTarball + "\n  signatures:\n    - https://sig.example/a.asc\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		"source key": {
			input:   "- name: " + urlTestTarball + "\n  source: https://galaxy.example\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		"namespace key": {
			input:   "- name: " + urlTestTarball + "\n  namespace: acme\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		"type url without a url name": {
			input:   "- name: acme.kafka\n  type: url\n",
			wantErr: helpers.ErrInvalidCollectionEntry,
		},
		"version range": {
			input:   "- name: " + urlTestTarball + "\n  version: \">=0.24.0\"\n",
			wantErr: helpers.ErrInvalidCollectionVersion,
		},
		"userinfo in the url": { //nolint:gosec // the fixture under test, not a credential
			input:   "- https://user:pw@h.example/x.tar.gz\n",
			wantErr: helpers.ErrURLRequirementUserinfo,
		},
		"fragment in the url": {
			input:   "- https://h.example/x.tar.gz#frag\n",
			wantErr: helpers.ErrInvalidURLRequirement,
		},
		"origin only": {
			input:   "- https://h.example\n",
			wantErr: helpers.ErrInvalidURLRequirement,
		},
		"caching-proxy path with a non-canonical upstream": {
			input:   "- http://proxy.example/https://GitHub.com/x.tar.gz\n",
			wantErr: helpers.ErrInvalidURLRequirement,
		},
		"url locator as a galaxy source": {
			input:   "- name: acme.kafka\n  type: galaxy\n  source: url+https://h.example/x.tar.gz#\n",
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte("collections:\n"+tc.input), "https://default")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestParseRolesAcceptsURLShapes pins the url role spellings: an http(s)
// .tar.gz src, a derived or explicit install name, an optional version label,
// and a github.com release asset, which the git promotion leaves to url.
func TestParseRolesAcceptsURLShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		input       string
		wantName    string
		wantVersion string
	}{
		"basename derives the install name": {
			input:    "roles:\n  - src: https://example.com/dl/myrole-1.2.3.tar.gz\n",
			wantName: "myrole-1.2.3",
		},
		"explicit name and version": {
			input:       "roles:\n  - src: https://example.com/dl/myrole-1.2.3.tar.gz\n    name: myrole\n    version: 1.2.3\n",
			wantName:    "myrole",
			wantVersion: "1.2.3",
		},
		"github release asset": {
			input:    "roles:\n  - src: https://github.com/acme/myrole/archive/1.2.3.tar.gz\n    name: myrole\n",
			wantName: "myrole",
		},
		"caching-proxy path derives the name from the upstream basename": {
			input:    "roles:\n  - src: http://cacheproxy.mirror.example.com/https://example.com/dl/myrole-1.2.3.tar.gz\n",
			wantName: "myrole-1.2.3",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file, err := Parse([]byte(tc.input), "https://default")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(file.Roles) != 1 {
				t.Fatalf("roles = %+v, want one entry", file.Roles)
			}
			role := file.Roles[0]
			if !role.IsURL() || role.Name != tc.wantName || role.Version != tc.wantVersion {
				t.Fatalf("url role shape = %+v, want name %q version %q", role, tc.wantName, tc.wantVersion)
			}
		})
	}
}

// TestParseRoleDependencyAcceptsURL pins that a meta-declared url dependency
// takes the same grammar a roles: entry does.
func TestParseRoleDependencyAcceptsURL(t *testing.T) {
	t.Parallel()
	req, skip, err := ParseRoleDependency(gitsource.RoleDependency{Src: "https://example.com/dl/dep-role.tar.gz"})
	if err != nil || skip != DependencyInstalled {
		t.Fatalf("ParseRoleDependency: skip=%v err=%v", skip, err)
	}
	if !req.IsURL() || req.Name != "dep-role" {
		t.Fatalf("url dependency shape = %+v", req)
	}
}

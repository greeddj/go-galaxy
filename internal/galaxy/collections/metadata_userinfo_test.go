package collections

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// metadataURLPassword is the credential these tests smuggle into a
// server-supplied metadata reference; it is distinctive so a substring search
// for it can only match the value itself.
const metadataURLPassword = "sup3rsecret"

// normalizeVersionsURLCase is one table entry for
// TestNormalizeVersionsURLRefusesUserinfo: a (source, versionsURL) pair and
// the sentinel it must carry, or nil for a pair that must be accepted.
type normalizeVersionsURLCase struct {
	wantErr     error
	name        string
	source      string
	versionsURL string
}

// normalizeVersionsURLCases pairs each userinfo shape (versions_url, the
// highest_version href, a relative reference inheriting the source's userinfo,
// caught only by checking the resolved output) with a credential-free twin.
func normalizeVersionsURLCases() []normalizeVersionsURLCase {
	const userinfo = "u:" + metadataURLPassword + "@"
	return []normalizeVersionsURLCase{
		{
			name:        "absolute versions_url with userinfo refused",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://" + userinfo + "hub.example/api/v3/collections/acme/widgets/versions/",
			wantErr:     helpers.ErrMetadataURLUserinfo,
		},
		{
			name:        "absolute versions_url without userinfo accepted",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://hub.example/api/v3/collections/acme/widgets/versions/",
			wantErr:     nil,
		},
		{
			name:        "highest_version href with userinfo refused",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://" + userinfo + "hub.example/api/v3/collections/acme/widgets/versions/1.0.0/",
			wantErr:     helpers.ErrMetadataURLUserinfo,
		},
		{
			name:        "highest_version href without userinfo accepted",
			source:      "https://hub.example/api/v3",
			versionsURL: "https://hub.example/api/v3/collections/acme/widgets/versions/1.0.0/",
			wantErr:     nil,
		},
		{
			name:        "relative reference against a source with userinfo refused",
			source:      "https://" + userinfo + "hub.example/api/v3",
			versionsURL: "collections/acme/widgets/versions/",
			wantErr:     helpers.ErrMetadataURLUserinfo,
		},
		{
			name:        "relative reference against a source without userinfo accepted",
			source:      "https://hub.example/api/v3",
			versionsURL: "collections/acme/widgets/versions/",
			wantErr:     nil,
		},
	}
}

// TestNormalizeVersionsURLRefusesUserinfo pins that normalizeVersionsURL
// refuses every userinfo shape in normalizeVersionsURLCases with
// helpers.ErrMetadataURLUserinfo and accepts each credential-free twin.
func TestNormalizeVersionsURLRefusesUserinfo(t *testing.T) {
	t.Parallel()

	for _, tt := range normalizeVersionsURLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeVersionsURL(tt.source, tt.versionsURL)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("normalizeVersionsURL(%q, %q) err = %v, want nil (got %q)", tt.source, tt.versionsURL, err, got)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("normalizeVersionsURL(%q, %q) err = %v, want %v", tt.source, tt.versionsURL, err, tt.wantErr)
			}
		})
	}
}

// TestNormalizeVersionsURLDoesNotEchoCredential pins that the metadata-URL
// refusal names the URL's origin and path but never its password, userinfo or
// query; the checks are independent t.Errorf calls so each leak is reported.
func TestNormalizeVersionsURLDoesNotEchoCredential(t *testing.T) {
	t.Parallel()

	raw := "https://u:" + metadataURLPassword + "@hub.example/api/v3/collections/acme/widgets/versions/" +
		"?X-Amz-Signature=deadbeef#frag"
	_, err := normalizeVersionsURL("https://hub.example/api/v3", raw)
	if err == nil {
		t.Fatal("normalizeVersionsURL accepted a userinfo-bearing metadata URL, want a refusal")
	}
	msg := err.Error()

	if strings.Contains(msg, metadataURLPassword) {
		t.Errorf("refusal message contains the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("refusal message contains the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("refusal message contains the presigned query: %s", msg)
	}
	if !strings.Contains(msg, "https://hub.example/api/v3/collections/acme/widgets/versions/") {
		t.Errorf("refusal message does not name the refused URL's origin and path: %s", msg)
	}
}

// TestNormalizeVersionsURLPassesThroughUnparseable pins that normalizeVersionsURL
// returns a value url.Parse refuses unchanged and without error; net/http later
// refuses to build a request from it, so nothing is fetched.
func TestNormalizeVersionsURLPassesThroughUnparseable(t *testing.T) {
	t.Parallel()

	// A raw control character in the authority makes url.Parse refuse it; it is
	// spliced in at run time because SA1007 flags a constant invalid URL.
	// #nosec G101 -- test fixture literal, not a real credential
	raw := "https://u:" + metadataURLPassword + "@hub.exa" + string([]byte{0x7f}) + "mple/versions/"
	if _, parseErr := url.Parse(raw); parseErr == nil {
		t.Fatalf("control: url.Parse(%q) succeeded, so this fixture does not reach the pass-through arm", raw)
	}

	got, err := normalizeVersionsURL("https://hub.example/api/v3", raw)
	if err != nil {
		t.Fatalf("normalizeVersionsURL(unparseable) err = %v, want nil", err)
	}
	if got != raw {
		t.Fatalf("normalizeVersionsURL(unparseable) = %q, want it returned unchanged: %q", got, raw)
	}
}

// unreachableCandidateMatch is a successMatch newFallThroughServer can never
// route to, so the server built with it answers 404 for every root-metadata
// candidate: no apiRoot candidate path contains it.
const unreachableCandidateMatch = "/no-such-candidate/"

// rootBodyWithVersionsURL renders the minimal root metadata document
// loadCollectionMetadata needs, pointing both the versions_url and the
// highest_version href at versionsURL.
func rootBodyWithVersionsURL(versionsURL string) string {
	return `{"versions_url":"` + versionsURL +
		`","highest_version":{"href":"` + versionsURL + `1.0.0/","version":"1.0.0"}}`
}

// TestLoadRootMetadataWalkUnaffectedByMetadataURLGuard pins that the guard sits
// downstream of the server walk: a 404 on the first server still advances to
// the second, whose credential-bearing versions_url is then refused.
func TestLoadRootMetadataWalkUnaffectedByMetadataURLGuard(t *testing.T) {
	t.Parallel()
	srvA, seenA := newFallThroughServer(t, unreachableCandidateMatch, "")
	poisoned := "https://u:" + metadataURLPassword + "@objects.example/api/v2/collections/acme/widgets/versions/"
	srvB, seenB := newFallThroughServer(t, "/api/v2/", rootBodyWithVersionsURL(poisoned))

	cfg := &config.Config{
		Server:  srvA.URL,
		Servers: []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL}},
	}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	// Both are plain HTTP httptest servers, so either server's client will do.
	runtime := infra.New(noopPrinter{}, srvB.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	_, err := loadCollectionMetadata(context.Background(), deps, col)
	if !errors.Is(err, helpers.ErrMetadataURLUserinfo) {
		t.Fatalf("loadCollectionMetadata err = %v, want helpers.ErrMetadataURLUserinfo", err)
	}
	if statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err); ok {
		t.Errorf("loadCollectionMetadata reported the first server's HTTP status (%d) instead of the guard's verdict: %v",
			statusErr.Code, err)
	}
	if len(seenA()) == 0 {
		t.Errorf("the first server received no request, so this run never exercised the walk at all")
	}
	if len(seenB()) == 0 {
		t.Errorf("the second server received no request, so the walk did not advance past the first server's 404")
	}

	assertCleanVersionsURLWalkSucceeds(t, srvA)
}

// assertCleanVersionsURLWalkSucceeds is the control for
// TestLoadRootMetadataWalkUnaffectedByMetadataURLGuard: the same walk with a
// credential-free versions_url pointing at a reachable third server succeeds.
func assertCleanVersionsURLWalkSucceeds(t *testing.T, srvA *httptest.Server) {
	t.Helper()
	// "{}" rather than a fuller document: the control only needs the version
	// metadata GET at the end of the walk to succeed, and every field this run
	// reads off it is optional.
	srvVersions, _ := newFallThroughServer(t, "/api/v2/", "{}")
	clean := srvVersions.URL + "/api/v2/collections/acme/widgets/versions/"
	srvB, _ := newFallThroughServer(t, "/api/v2/", rootBodyWithVersionsURL(clean))

	cfg := &config.Config{
		Server:  srvA.URL,
		Servers: []config.Server{{ID: "a", URL: srvA.URL}, {ID: "b", URL: srvB.URL}},
	}
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	runtime := infra.New(noopPrinter{}, srvB.Client())
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, err := loadCollectionMetadata(context.Background(), deps, col); err != nil {
		t.Fatalf("control: loadCollectionMetadata with a credential-free versions_url = %v, want nil", err)
	}
}

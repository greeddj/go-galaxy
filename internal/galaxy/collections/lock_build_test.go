package collections

// This file pins buildLockfile's guards: lock manufactures the pin, so a
// lying server's bad digest or download URL, or a poisoned snapshot's
// non-exact version, must never reach a committed lockfile.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// testWidgetsFQDN is the fqdn key buildLockfile's resolved map uses for the
// acme.widgets fixture every test in this file shares.
const testWidgetsFQDN = "acme.widgets"

// metadataRewritingTransport applies rewrite to the decoded body of every OK
// response whose path contains pathMarker, so a lying server's answer reaches
// loadCollectionMetadata without giving fakegalaxy a body-tampering hook.
type metadataRewritingTransport struct {
	base       http.RoundTripper
	rewrite    func(body map[string]any)
	pathMarker string
}

func (t *metadataRewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || !strings.Contains(req.URL.Path, t.pathMarker) {
		return resp, err
	}
	var body map[string]any
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		return nil, decErr
	}
	_ = resp.Body.Close()
	t.rewrite(body)
	rewritten, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return nil, marshalErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return resp, nil
}

// newBuildLockfileFixture serves acme.widgets@1.0.0 from fakegalaxy through a
// client that rewrites its artifact.sha256 to replacement, and returns the
// deps, resolved and graph arguments buildLockfile takes.
func newBuildLockfileFixture(t *testing.T, replacement string) (collectionDeps, map[string]collection, map[string][]string) {
	t.Helper()
	deps, resolved, graph, _ := newRewrittenBuildLockfileFixture(t, func(_ string, body map[string]any) {
		if artifact, ok := body["artifact"].(map[string]any); ok {
			artifact["sha256"] = replacement
		}
	})
	return deps, resolved, graph
}

// newRewrittenBuildLockfileFixture is newBuildLockfileFixture with rewrite
// applied to the version document, handed the fake server's URL, and the
// registered version returned too.
func newRewrittenBuildLockfileFixture(
	t *testing.T, rewrite func(server string, body map[string]any),
) (collectionDeps, map[string]collection, map[string][]string, fakegalaxy.Version) {
	t.Helper()
	srv := fakegalaxy.New(t)
	version := srv.AddVersion("acme", "widgets", testVersion100, nil)

	// fakegalaxy serves plain http, so http.DefaultTransport reaches it.
	client := &http.Client{
		Transport: &metadataRewritingTransport{
			base:       http.DefaultTransport,
			rewrite:    func(body map[string]any) { rewrite(srv.URL(), body) },
			pathMarker: "/versions/" + testVersion100 + "/",
		},
	}

	cfg := &config.Config{Server: srv.URL(), Workers: 1}
	runtime := infra.New(noopPrinter{}, client)
	st := store.New()
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	resolved := map[string]collection{testWidgetsFQDN: col}
	graph := map[string][]string{col.key(): {}}
	return newCollectionDeps(cfg, runtime, st), resolved, graph, version
}

// TestBuildLockfileRejectsNonCanonicalDigest pins that an uppercase-hex
// server digest fails buildLockfile with helpers.ErrMalformedArtifactSHA256
// and yields no lockfile, rather than committing the bad digest.
func TestBuildLockfileRejectsNonCanonicalDigest(t *testing.T) {
	t.Parallel()
	const nonCanonical = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	deps, resolved, graph := newBuildLockfileFixture(t, nonCanonical)

	lf, err := buildLockfile(context.Background(), deps, resolved, graph, roleResolution{})
	if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
		t.Fatalf("buildLockfile error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
	}
	if lf != nil {
		t.Fatalf("buildLockfile lockfile = %+v, want nil: no lockfile must be written on rejection", lf)
	}
}

// TestBuildLockfileRejectsNonExactVersion pins that a resolved Version "*", as
// a poisoned snapshot entry can carry, fails with ErrInvalidCollectionVersion
// before any metadata fetch (srv.Total() == 0) and yields no lockfile.
func TestBuildLockfileRejectsNonExactVersion(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)
	cfg := &config.Config{Server: srv.URL(), Workers: 1}
	runtime := infra.New(noopPrinter{}, srv.Client())
	st := store.New()
	col := collection{Namespace: "acme", Name: "widgets", Version: "*"}
	resolved := map[string]collection{testWidgetsFQDN: col}
	graph := map[string][]string{col.key(): {}}

	lf, err := buildLockfile(context.Background(), newCollectionDeps(cfg, runtime, st), resolved, graph, roleResolution{})
	if !errors.Is(err, helpers.ErrInvalidCollectionVersion) {
		t.Fatalf("buildLockfile error = %v, want errors.Is helpers.ErrInvalidCollectionVersion", err)
	}
	if lf != nil {
		t.Fatalf("buildLockfile lockfile = %+v, want nil: no lockfile must be written on rejection", lf)
	}
	if got := srv.Total(); got != 0 {
		t.Errorf("srv.Total() = %d, want 0 (the version guard must reject before any metadata fetch)", got)
	}
}

// TestBuildLockfileAcceptsEmptyDigest pins that an empty digest, from a server
// that publishes none, yields an entry with an empty pin and no error, since
// verifyPinnedSHA reads an empty pin as no pin at all.
func TestBuildLockfileAcceptsEmptyDigest(t *testing.T) {
	t.Parallel()
	deps, resolved, graph := newBuildLockfileFixture(t, "")

	lf, err := buildLockfile(context.Background(), deps, resolved, graph, roleResolution{})
	if err != nil {
		t.Fatalf("buildLockfile error = %v, want nil for an empty (unpublished) digest", err)
	}
	if len(lf.Collections) != 1 {
		t.Fatalf("unexpected lockfile collections: %+v, want exactly 1 entry", lf.Collections)
	}
	entry := lf.Collections[0]
	if entry.Name != testWidgetsFQDN || entry.Version != testVersion100 {
		t.Fatalf("unexpected lockfile entry: %+v, want %s@%s", entry, testWidgetsFQDN, testVersion100)
	}
	if entry.SHA256 != "" {
		t.Fatalf("lockfile entry SHA256 = %q, want empty", entry.SHA256)
	}
}

// TestBuildLockfileWritesTheServerDownloadURL pins that a Galaxy entry locks
// the download URL its server named, in canonical form: a fragment, which
// never reaches a server, is dropped rather than committed.
func TestBuildLockfileWritesTheServerDownloadURL(t *testing.T) {
	t.Parallel()
	deps, resolved, graph, version := newRewrittenBuildLockfileFixture(t, func(_ string, body map[string]any) {
		if raw, ok := body["download_url"].(string); ok {
			body["download_url"] = raw + "#part"
		}
	})

	lf, err := buildLockfile(context.Background(), deps, resolved, graph, roleResolution{})
	if err != nil {
		t.Fatalf("buildLockfile error = %v, want nil", err)
	}
	if got := lf.Collections[0].DownloadURL; got != version.DownloadURL {
		t.Fatalf("lockfile entry DownloadURL = %q, want the server's %q without its fragment", got, version.DownloadURL)
	}
}

// unlockableDownloadURLCase is one row of
// TestBuildLockfileRefusesAnUnlockableDownloadURL: the download URL a server
// names, built from the fake server's URL, and the sentinel refusing it.
type unlockableDownloadURLCase struct {
	want error
	url  func(server string) string
	name string
}

// presignedSignature is the capability a presigned query carries, which no
// refusal may echo.
const presignedSignature = "X-Amz-Signature=deadbeefcafe"

// unlockableDownloadURLCases gives each refusal lock applies to a server's
// download URL a row of its own.
func unlockableDownloadURLCases() []unlockableDownloadURLCase {
	const artifact = "/download/acme-widgets-1.0.0.tar.gz"
	return []unlockableDownloadURLCase{
		{name: "missing", want: helpers.ErrMissingDownloadURL, url: func(string) string { return "" }},
		{name: "not http", want: helpers.ErrUnsupportedDownloadURLScheme, url: func(string) string { return "file:///etc/passwd" }},
		{name: "userinfo", want: helpers.ErrDownloadURLUserinfo, url: func(s string) string {
			return strings.Replace(s, "://", "://u:p@", 1) + artifact
		}},
		{name: "presigned query", want: helpers.ErrDownloadURLQuery, url: func(s string) string {
			return s + artifact + "?" + presignedSignature
		}},
		{name: "another origin", want: helpers.ErrDownloadURLNotServerArtifact, url: func(string) string {
			return "https://cdn.example.invalid" + artifact
		}},
		{name: "another artifact", want: helpers.ErrDownloadURLNotServerArtifact, url: func(s string) string {
			return s + "/download/acme-other-1.0.0.tar.gz"
		}},
	}
}

// TestBuildLockfileRefusesAnUnlockableDownloadURL pins that lock refuses a
// download URL it could not commit or a frozen install could not trust, with
// no lockfile written and no presigned query echoed.
func TestBuildLockfileRefusesAnUnlockableDownloadURL(t *testing.T) {
	t.Parallel()
	for _, tc := range unlockableDownloadURLCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, resolved, graph, _ := newRewrittenBuildLockfileFixture(t, func(server string, body map[string]any) {
				body["download_url"] = tc.url(server)
			})

			lf, err := buildLockfile(context.Background(), deps, resolved, graph, roleResolution{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("buildLockfile error = %v, want errors.Is %v", err, tc.want)
			}
			if lf != nil {
				t.Fatalf("buildLockfile lockfile = %+v, want nil: no lockfile must be written on rejection", lf)
			}
			if strings.Contains(err.Error(), presignedSignature) {
				t.Fatalf("refusal echoes the presigned query: %v", err)
			}
		})
	}
}

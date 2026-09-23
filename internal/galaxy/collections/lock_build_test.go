package collections

// This file pins buildLockfile's guards: lock manufactures the pin, so a
// non-canonical digest from a lying server or a non-exact version from a
// poisoned snapshot must never reach a committed lockfile.

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

// sha256RewritingTransport rewrites artifact.sha256 to replacement in every
// OK response whose path contains pathMarker, so a bad digest reaches
// loadCollectionMetadata without giving fakegalaxy a body-tampering hook.
type sha256RewritingTransport struct {
	base        http.RoundTripper
	pathMarker  string
	replacement string
}

func (t *sha256RewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK || !strings.Contains(req.URL.Path, t.pathMarker) {
		return resp, err
	}
	var body map[string]any
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		return nil, decErr
	}
	_ = resp.Body.Close()
	if artifact, ok := body["artifact"].(map[string]any); ok {
		artifact["sha256"] = t.replacement
	}
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
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", testVersion100, nil)

	// fakegalaxy serves plain http, so http.DefaultTransport reaches it.
	client := &http.Client{
		Transport: &sha256RewritingTransport{
			base:        http.DefaultTransport,
			pathMarker:  "/versions/" + testVersion100 + "/",
			replacement: replacement,
		},
	}

	cfg := &config.Config{Server: srv.URL(), Workers: 1}
	runtime := infra.New(noopPrinter{}, client)
	st := store.New()
	col := collection{Namespace: "acme", Name: "widgets", Version: testVersion100}
	resolved := map[string]collection{testWidgetsFQDN: col}
	graph := map[string][]string{col.key(): {}}
	return newCollectionDeps(cfg, runtime, st), resolved, graph
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

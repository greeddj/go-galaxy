package collections

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestGalaxyLockfileEntryReadsA404AsNoCandidate pins that lock reads a 404 for
// a collection or version the resolve named as the source lacking it, exit 3's
// sentinel, rather than an HTTP status no exit class claims.
func TestGalaxyLockfileEntryReadsA404AsNoCandidate(t *testing.T) {
	t.Parallel()
	const rootPath = "/api/v3/collections/acme/widgets/"
	for name, versionGone := range map[string]bool{"collection gone": false, "version gone": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var versionAsked atomic.Bool
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case versionGone && r.URL.Path == rootPath:
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"versions_url":"%[1]s%[2]sversions/",`+
						`"highest_version":{"href":"%[1]s%[2]sversions/1.0.0/","version":"1.0.0"}}`, srv.URL, rootPath)
				case r.URL.Path == rootPath+"versions/1.0.0/":
					versionAsked.Store(true)
					w.WriteHeader(http.StatusNotFound)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)

			col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
			_, err := galaxyLockfileEntry(t.Context(), webUIRootDeps(srv), "acme.widgets", col, nil)
			if !errors.Is(err, helpers.ErrNoSemverCandidates) {
				t.Fatalf("galaxyLockfileEntry error = %v, want helpers.ErrNoSemverCandidates", err)
			}
			if versionAsked.Load() != versionGone {
				t.Fatalf("version document requested = %t, want %t", versionAsked.Load(), versionGone)
			}
		})
	}
}

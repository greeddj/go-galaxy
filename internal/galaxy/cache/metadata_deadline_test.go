package cache

// This file covers helpers.MetadataFetchDeadline against a byte-drip fixture
// that always makes progress, so only a whole-request deadline can catch it.
// Its one-request count does not pin the budget's scope or class on its own.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// metadataDripInterval is the fixture's drip cadence: several bytes land well
// inside metadataDripBudget, yet the document never completes.
const metadataDripInterval = 5 * time.Millisecond

// metadataDripBudget is the fetchJSONBody budget every test in this file
// passes.
const metadataDripBudget = 200 * time.Millisecond

// newMetadataDripServer serves "{" then one space per metadataDripInterval
// until the request ends when drip is true, else a small valid JSON body;
// requests counts every request received.
func newMetadataDripServer(t *testing.T, drip bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !drip {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if _, err := w.Write([]byte("{")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(metadataDripInterval):
			}
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// TestMetadataByteDripFailsAtTheFetchDeadline pins that a byte drip fails with
// helpers.ErrMetadataFetchDeadline, matching neither context.DeadlineExceeded
// nor context.Canceled, after exactly one request.
func TestMetadataByteDripFailsAtTheFetchDeadline(t *testing.T) {
	t.Parallel()
	srv, requests := newMetadataDripServer(t, true)

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, nil, &out, Policy{}, metadataDripBudget)
	if err == nil {
		t.Fatal("expected an error from a byte-dripped metadata response, got nil")
	}
	if !errors.Is(err, helpers.ErrMetadataFetchDeadline) {
		t.Fatalf("expected errors.Is ErrMetadataFetchDeadline, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("must not match context.DeadlineExceeded: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (the deadline is terminal: no retry follows it)", got)
	}
}

// TestMetadataDripFixturePositiveControl pins that the same fixture without
// the drip decodes under the same budget, so the budget alone does not fail
// TestMetadataByteDripFailsAtTheFetchDeadline.
func TestMetadataDripFixturePositiveControl(t *testing.T) {
	t.Parallel()
	srv, requests := newMetadataDripServer(t, false)

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL, nil, &out, Policy{}, metadataDripBudget)
	if err != nil {
		t.Fatalf("FetchJSONWithCachePolicy: %v", err)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("expected out to decode {ok:true}, got %v", out)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

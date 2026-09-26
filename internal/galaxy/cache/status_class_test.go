package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// statusFixtureURL is the cut URL every status fixture here names.
const statusFixtureURL = "https://hub.example/api/v3/collections/acme/widgets/versions/"

// TestHTTPStatusErrorCarriesItsClass pins StatusClass, Unwrap and Error over
// one table: a 404 carries no class and keeps its old text, 401 and 403 are
// auth, and every other status is unavailable, its text named exactly once.
func TestHTTPStatusErrorCarriesItsClass(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		want error
		code int
	}{
		{code: http.StatusNotFound},
		{code: http.StatusNotModified, want: helpers.ErrGalaxyServerUnavailable},
		{code: http.StatusUnauthorized, want: helpers.ErrGalaxyAuthFailed},
		{code: http.StatusForbidden, want: helpers.ErrGalaxyAuthFailed},
		{code: http.StatusBadRequest, want: helpers.ErrGalaxyServerUnavailable},
		{code: http.StatusMethodNotAllowed, want: helpers.ErrGalaxyServerUnavailable},
		{code: http.StatusGone, want: helpers.ErrGalaxyServerUnavailable},
		{code: http.StatusTooManyRequests, want: helpers.ErrGalaxyServerUnavailable},
		{code: http.StatusInternalServerError, want: helpers.ErrGalaxyServerUnavailable},
		{code: http.StatusServiceUnavailable, want: helpers.ErrGalaxyServerUnavailable},
	} {
		status := fmt.Sprintf("%d %s", tt.code, http.StatusText(tt.code))
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			if got := StatusClass(tt.code); !errors.Is(got, tt.want) {
				t.Errorf("StatusClass(%d) = %v, want %v", tt.code, got, tt.want)
			}
			statusErr := &HTTPStatusError{URL: statusFixtureURL, Status: status, Code: tt.code}
			wrapped := fmt.Errorf("fetching dependencies of acme.widgets@1.0.0: %w", statusErr)
			assertStatusClass(t, wrapped, tt.want)
			if got, ok := errors.AsType[*HTTPStatusError](wrapped); !ok || got.Code != tt.code {
				t.Errorf("errors.AsType lost the status error or its Code: %v", wrapped)
			}
			bare := "failed to fetch metadata: " + status + " (" + statusFixtureURL + ")"
			want := bare
			if tt.want != nil {
				want = tt.want.Error() + ": " + bare
			}
			if got := statusErr.Error(); got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
		})
	}
}

// assertStatusClass fails unless err carries want and no other class sentinel,
// or, for a nil want, neither class sentinel; a class's text appears once.
func assertStatusClass(t *testing.T, err, want error) {
	t.Helper()
	for _, class := range []error{helpers.ErrGalaxyAuthFailed, helpers.ErrGalaxyServerUnavailable} {
		isWant := errors.Is(class, want)
		if got := errors.Is(err, class); got != isWant {
			t.Errorf("errors.Is(%v, %v) = %t, want %t", err, class, got, isWant)
		}
		wantCount := 0
		if isWant {
			wantCount = 1
		}
		if got := strings.Count(err.Error(), class.Error()); got != wantCount {
			t.Errorf("%q names %q %d times, want %d", err.Error(), class.Error(), got, wantCount)
		}
	}
}

// TestMetadataWrongShapeIsMalformed pins that JSON not fitting its document, a
// value of the wrong type or a timestamp time.Time refuses, is ErrMetadataMalformed
// naming the cut URL, never a web page or ErrMetadataNotJSON, and is not cached.
func TestMetadataWrongShapeIsMalformed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		out   func() any
		cause func(error) bool
		name  string
		body  string
	}{
		{name: "array for an object", body: `[]`, out: func() any { return &map[string]any{} }, cause: isTypeError},
		{name: "string for a struct", body: `{"highest_version":"1.0.0"}`, out: func() any {
			return &struct {
				HighestVersion struct {
					Version string `json:"version"`
				} `json:"highest_version"`
			}{}
		}, cause: isTypeError},
		{name: "timestamp that does not parse", body: `{"created_at":"garbage"}`, out: newTimestamped, cause: isTimeError},
		{name: "timestamp out of range", body: `{"created_at":"2020-01-01T00:00:00+25:00"}`, out: newTimestamped, cause: isTimeError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := newBodyServer(t, "application/json", tt.body)
			raw := srv.URL + "/api/v3/collections/acme/widgets/?token=" + classifyFixtureSecret
			st := store.New()
			policy := Policy{Read: true, Write: true, TTL: time.Minute}
			err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), raw, st, tt.out(), policy, 0)
			if !errors.Is(err, helpers.ErrMetadataMalformed) {
				t.Fatalf("err = %v, want errors.Is helpers.ErrMetadataMalformed", err)
			}
			if errors.Is(err, helpers.ErrMetadataNotJSON) || IsWebPage(err) {
				t.Errorf("err = %v: wrong-shape JSON is not a body that is no JSON", err)
			}
			if !tt.cause(err) {
				t.Errorf("err = %v, want the decoder's own error still reachable", err)
			}
			if strings.Contains(err.Error(), classifyFixtureSecret) || !strings.Contains(err.Error(), "/api/v3/collections/acme/widgets/") {
				t.Errorf("err = %v, want the URL named with its query cut", err)
			}
			if _, ok := st.GetAPICache(apiCacheKey(raw)); ok {
				t.Error("a body that did not decode was stored in the API cache")
			}
		})
	}
}

// newTimestamped returns a document carrying one time.Time, as every Galaxy
// collection document does.
func newTimestamped() any {
	return &struct {
		CreatedAt time.Time `json:"created_at"`
	}{}
}

// isTypeError reports whether err carries a decoder's *json.UnmarshalTypeError.
func isTypeError(err error) bool {
	_, ok := errors.AsType[*json.UnmarshalTypeError](err)
	return ok
}

// isTimeError reports whether err carries time.Time's own *time.ParseError.
func isTimeError(err error) bool {
	_, ok := errors.AsType[*time.ParseError](err)
	return ok
}

// TestDecodeMetadataNonPointerStaysBare pins the one decoder failure that is no
// document's fault: an out json.Unmarshal cannot write is the caller's defect.
func TestDecodeMetadataNonPointerStaysBare(t *testing.T) {
	t.Parallel()
	var out map[string]any
	err := decodeMetadata(statusFixtureURL, []byte(`{}`), out)
	if _, ok := errors.AsType[*json.InvalidUnmarshalError](err); !ok || errors.Is(err, helpers.ErrMetadataMalformed) {
		t.Fatalf("err = %v, want a bare *json.InvalidUnmarshalError", err)
	}
}

// TestUnaskedNotModifiedIsAStatus pins that a 304 to a request carrying no
// validator, with no cached copy to keep, is a status carrying its class, not
// an empty body read as a document that is no JSON.
func TestUnaskedNotModifiedIsAStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), srv.Client(), srv.URL+"/api/", store.New(), &out, Policy{Read: true, Write: true}, 0)
	statusErr, ok := errors.AsType[*HTTPStatusError](err)
	if !ok || statusErr.Code != http.StatusNotModified || errors.Is(err, helpers.ErrMetadataNotJSON) {
		t.Fatalf("err = %v, want an HTTPStatusError for the 304", err)
	}
	assertStatusClass(t, err, helpers.ErrGalaxyServerUnavailable)
}

// TestDecodeMetadataSyntaxErrorStaysNotJSON pins the other half of the split:
// a body that is no JSON at all is still helpers.ErrMetadataNotJSON alone.
func TestDecodeMetadataSyntaxErrorStaysNotJSON(t *testing.T) {
	t.Parallel()
	var out map[string]any
	err := decodeMetadata(statusFixtureURL, []byte(`{"highest_version":`), &out)
	if !errors.Is(err, helpers.ErrMetadataNotJSON) || errors.Is(err, helpers.ErrMetadataMalformed) {
		t.Fatalf("err = %v, want helpers.ErrMetadataNotJSON and not helpers.ErrMetadataMalformed", err)
	}
}

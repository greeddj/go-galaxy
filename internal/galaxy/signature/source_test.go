package signature

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// sourceTestTimeout is what every Fetcher here is built with, generous on
	// purpose: nothing here measures a timeout, and a tight one would flake.
	sourceTestTimeout = 5 * time.Second

	// sourceBlobBody is the payload every accepting case transfers; it is never
	// verified as a signature, since this file covers only the fetch.
	sourceBlobBody = "-----BEGIN PGP SIGNATURE-----\n\naGVsbG8=\n-----END PGP SIGNATURE-----\n"

	// sigLeafName is the leaf every file fixture here is written to, and the
	// path every http fixture is requested under.
	sigLeafName = "sig.asc"

	// presignedQuery is the shape this project already treats as a bearer
	// capability elsewhere: a presigned object-storage URL's query string.
	presignedQuery = "?X-Amz-Signature=deadbeef"

	// serverBodyMarker is a token no error message may carry. It stands in for
	// whatever an error page, a proxy, or a login form puts in a non-200 body.
	serverBodyMarker = "body-marker-that-must-not-be-printed"

	// sourcePassword is the password every credentialed fixture carries and the
	// token messages are searched for, spelled once so the two cannot differ.
	sourcePassword = "s3cr3t"

	// unavailablePrefix and unreadableSuffix spell the file arm's one message
	// by hand: an expectation built from the production format string cannot
	// catch a change to it.
	unavailablePrefix = `collection signature source unavailable: "`
	unreadableSuffix  = `" could not be read`

	// requestLogDepth is how many requests a fixture server records before a
	// send would block. Every test here makes one or two.
	requestLogDepth = 4

	// tinySourceLimit is the blob ceiling the size-cap tests run against, small
	// enough that both sides of it are spelled as literal bodies.
	tinySourceLimit = 64
)

// writeSourceFile writes body at dir/name and returns the absolute path.
func writeSourceFile(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), helpers.FileMod); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	return path
}

// newSourceServer answers every request with 200 and body, and hands back the
// RequestURI of each one it served so a test can assert what actually went out
// on the wire rather than what the caller passed in.
func newSourceServer(t *testing.T, body string) (*httptest.Server, chan string) {
	t.Helper()

	seen := make(chan string, requestLogDepth)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.RequestURI()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv, seen
}

// newFailingServer answers every request with status and a body carrying
// serverBodyMarker, so a test can prove that body never reaches a message.
func newFailingServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(serverBodyMarker))
	}))
	t.Cleanup(srv.Close)

	return srv
}

// newTestFetcher builds a Fetcher at the given ceiling. Tests that do not
// exercise the ceiling pass helpers.SignatureMaxSize, so the seam is only ever
// used to shrink the real value rather than to change what is under test.
func newTestFetcher(offline bool, limit int64) *Fetcher {
	return newFetcher(sourceTestTimeout, offline, limit)
}

// TestNewFetcherCarriesTheRealCeiling pins that the public constructor uses
// helpers.SignatureMaxSize, which every other test bypasses through the
// shrinking seam, and that the client it builds can fetch.
func TestNewFetcherCarriesTheRealCeiling(t *testing.T) {
	t.Parallel()

	f := NewFetcher(sourceTestTimeout, false)
	if f.limit != helpers.SignatureMaxSize {
		t.Fatalf("NewFetcher limit = %d, want helpers.SignatureMaxSize (%d)", f.limit, helpers.SignatureMaxSize)
	}
	if f.offline {
		t.Fatal("NewFetcher(offline=false) built an offline fetcher")
	}

	srv, _ := newSourceServer(t, sourceBlobBody)
	blob, err := f.FetchRequirementSource(t.Context(), srv.URL+"/"+sigLeafName)
	if err != nil {
		t.Fatalf("FetchRequirementSource error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("data = %q, want the fixture blob", blob.Data)
	}
}

// TestFetchRequirementSourceAcceptsEveryFetchableSpelling is the positive
// control for every refusal here: the four RFC 8089 file spellings (LocalHost
// pins the case-insensitive host compare) and an http URL are all fetched.
func TestFetchRequirementSourceAcceptsEveryFetchableSpelling(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)
	srv, _ := newSourceServer(t, sourceBlobBody)

	cases := []struct {
		name   string
		source string
	}{
		{name: "file url with an empty authority", source: "file://" + path},
		{name: "file url naming localhost", source: "file://localhost" + path},
		{name: "file url naming LocalHost", source: "file://LocalHost" + path},
		{name: "file url with a single slash", source: "file:" + path},
		{name: "http url", source: srv.URL + "/" + sigLeafName},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if err != nil {
				t.Fatalf("FetchRequirementSource(%q) error = %v, want nil", tc.source, err)
			}
			if string(blob.Data) != sourceBlobBody {
				t.Fatalf("FetchRequirementSource(%q) data = %q, want the fixture blob", tc.source, blob.Data)
			}
			if blob.Origin != tc.source {
				t.Fatalf("FetchRequirementSource(%q) origin = %q, want the source itself", tc.source, blob.Origin)
			}
		})
	}
}

// TestFetchRequirementSourceRefusesEveryUnfetchableShape pins the
// unsupported-source sentinel for a foreign or missing scheme, a file URL
// naming another host, a relative path or no path, and an unparseable value.
func TestFetchRequirementSourceRefusesEveryUnfetchableShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		source string
	}{
		{name: "ftp scheme", source: "ftp://h/x"},
		{name: "git+ssh scheme", source: "git+ssh://h/x"},
		{name: "data scheme", source: "data:text/plain;base64,aGk="},
		{name: "bare absolute path", source: "/bare/abs/path"},
		{name: "relative path", source: "relative/path"},
		{name: "empty", source: ""},
		{name: "scheme-relative url", source: "//host/path"},
		{name: "file url naming a foreign host", source: "file://evil.example/x"},
		{name: "file url with a relative path", source: "file:relative/x"},
		{name: "file url naming localhost with no path", source: "file://localhost"},
		{name: "unparseable url", source: "http://%zz/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The relative and data rows are refused twice over, by the opaque
			// check and by a later arm, so no single deletion moves either one.
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
				t.Logf("error = %v", err)
				t.Fatalf("%s was not refused as unfetchable", tc.name)
			}
		})
	}
}

// TestFetchFileRefusalsAreAboutTheSpellingNotTheFile reads one real file by its
// localhost URL and refuses the foreign-authority and relative spellings of the
// same path, so the refusals are about the URL rather than a missing file.
func TestFetchFileRefusalsAreAboutTheSpellingNotTheFile(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)
	fetcher := newTestFetcher(false, helpers.SignatureMaxSize)

	blob, err := fetcher.FetchRequirementSource(t.Context(), "file://localhost"+path)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(a localhost file url) error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	for _, source := range []string{"file://evil.example" + path, "file:" + strings.TrimPrefix(path, "/")} {
		if _, err := fetcher.FetchRequirementSource(t.Context(), source); !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
			t.Fatalf("FetchRequirementSource(%q) error = %v, want the unsupported-source sentinel", source, err)
		}
	}
}

// TestFetchRequirementSourceRefusesUserinfo pins that a source carrying
// userinfo is refused without printing the credential, while the same URL
// without it is fetched.
func TestFetchRequirementSourceRefusesUserinfo(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	clean := srv.URL + "/" + sigLeafName
	credentialed := strings.Replace(clean, "http://", "http://user:pass@", 1) + presignedQuery

	// net/http would turn the userinfo into Basic auth and the fixture answers
	// 200, so only the refusal keeps this fetch from succeeding.
	_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), credentialed)
	if !errors.Is(err, helpers.ErrSignatureSourceUserinfo) {
		t.Fatalf("a source carrying userinfo: error = %v, want the userinfo sentinel", err)
	}

	for _, secret := range []string{"pass", "user:pass", "user@", "X-Amz-Signature"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("refusal message %q carries %q, which it exists to keep out of every sink", err, secret)
		}
	}

	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), clean)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", clean, err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}
}

// TestFetchRequirementSourceNeverPrintsACredential covers credentialed values
// url.Parse refuses or reports as opaque: each refusal omits the password yet
// still names the host, which holds because display precedes the parse.
func TestFetchRequirementSourceNeverPrintsACredential(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	clean := srv.URL + "/" + sigLeafName
	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), clean)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", clean, err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	cases := []struct {
		name     string
		source   string
		wantHost string
	}{
		{name: "invalid port", source: "https://user:" + sourcePassword + "@host:notaport/sig.asc", wantHost: "host:notaport"},
		{name: "bad percent escape", source: "https://user:" + sourcePassword + "@host/%zz/sig.asc", wantHost: "host"},
		{name: "invalid ip literal", source: "https://user:" + sourcePassword + "@[::1x]/sig.asc", wantHost: "[::1x]"},
		{name: "control character", source: "https://user:" + sourcePassword + "@host/sig\x00.asc", wantHost: "host"},
		// An opaque URL left to http.Client fails as "no Host in request URL",
		// a transport failure a CI retries; it must be an unsupported source.
		{name: "opaque url", source: "http:user:" + sourcePassword + "@host/sig.asc", wantHost: "host"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The error goes to the log, never the failure message: one row
			// carries a NUL byte, another an OS rendering of a malformed address.
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
				t.Logf("error = %v", err)
				t.Fatalf("%s was not refused as an unsupported source", tc.name)
			}
			if strings.Contains(err.Error(), sourcePassword) {
				t.Fatalf("refusal message carries %q", sourcePassword)
			}
			if !strings.Contains(err.Error(), tc.wantHost) {
				t.Logf("message = %q", err)
				t.Fatalf("%s: refusal message does not name the host, so it names nothing an operator can act on", tc.name)
			}
		})
	}
}

// TestFetchStripsQueryFromOriginAndErrors pins the query cut's asymmetry: the
// request carries the query, which may be the capability making the source
// fetchable, while Blob.Origin and every error are cut at it.
func TestFetchStripsQueryFromOriginAndErrors(t *testing.T) {
	t.Parallel()

	srv, seen := newSourceServer(t, sourceBlobBody)
	clean := srv.URL + "/" + sigLeafName

	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), clean+presignedQuery)
	if err != nil {
		t.Fatalf("FetchRequirementSource error = %v, want nil", err)
	}
	if got := <-seen; !strings.Contains(got, strings.TrimPrefix(presignedQuery, "?")) {
		t.Fatalf("server saw RequestURI %q, want the query carried through to the wire", got)
	}

	// The rendered values go to the log: both carry the fixture's own port.
	if blob.Origin != clean {
		t.Logf("origin = %q, want %q", blob.Origin, clean)
		t.Fatalf("Blob.Origin kept the capability query")
	}

	failing := newFailingServer(t, http.StatusNotFound)
	_, err = newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(t.Context(), failing.URL+"/"+sigLeafName+presignedQuery)
	if err == nil {
		t.Fatal("FetchRequirementSource error = nil, want a refusal from the 404 fixture")
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Fatalf("error message %q carries the capability query", err)
	}
}

// TestFetchNon200NamesOnlyTheStatus pins what a refusal from an answering
// server is allowed to say. The body is bytes chosen by whoever answered, so it
// is neither read nor rendered; the status code is what an operator can act on.
func TestFetchNon200NamesOnlyTheStatus(t *testing.T) {
	t.Parallel()

	srv := newFailingServer(t, http.StatusForbidden)

	_, err := newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(t.Context(), srv.URL+"/"+sigLeafName)
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource error = %v, want the unavailable sentinel", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error message %q does not name the status code", err)
	}
	if strings.Contains(err.Error(), serverBodyMarker) {
		t.Fatalf("error message %q carries the response body, which is never read", err)
	}
}

// TestFetchFileSizeCeiling pins both sides of the file path's ceiling: a file
// of exactly the ceiling is accepted and one byte more is refused.
func TestFetchFileSizeCeiling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	exact := writeSourceFile(t, dir, "exact.asc", strings.Repeat("a", tinySourceLimit))
	over := writeSourceFile(t, dir, "over.asc", strings.Repeat("a", tinySourceLimit+1))

	blob, err := newTestFetcher(false, tinySourceLimit).FetchRequirementSource(t.Context(), "file://"+exact)
	if err != nil {
		t.Fatalf("a file of exactly the ceiling: error = %v, want nil", err)
	}
	if len(blob.Data) != tinySourceLimit {
		t.Fatalf("a file of exactly the ceiling: read %d bytes, want %d", len(blob.Data), tinySourceLimit)
	}

	// A read of exactly f.limit bytes would end on a clean EOF and accept this
	// file, which is why the file path reads one byte past the ceiling.
	_, err = newTestFetcher(false, tinySourceLimit).FetchRequirementSource(t.Context(), "file://"+over)
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("a file one byte over the ceiling: error = %v, want the unavailable sentinel", err)
	}
}

// TestFetchHTTPSizeCeiling is the file ceiling's http counterpart, a separate
// test because helpers.NewSizeLimitedReader fails on the first byte past the
// ceiling instead of reading one extra byte.
func TestFetchHTTPSizeCeiling(t *testing.T) {
	t.Parallel()

	exactSrv, _ := newSourceServer(t, strings.Repeat("a", tinySourceLimit))
	blob, err := newTestFetcher(false, tinySourceLimit).
		FetchRequirementSource(t.Context(), exactSrv.URL+"/"+sigLeafName)
	if err != nil {
		t.Fatalf("a body of exactly the ceiling: error = %v, want nil", err)
	}
	if len(blob.Data) != tinySourceLimit {
		t.Fatalf("a body of exactly the ceiling: read %d bytes, want %d", len(blob.Data), tinySourceLimit)
	}

	overSrv, _ := newSourceServer(t, strings.Repeat("a", tinySourceLimit+1))
	_, err = newTestFetcher(false, tinySourceLimit).
		FetchRequirementSource(t.Context(), overSrv.URL+"/"+sigLeafName)
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("a body one byte over the ceiling: error = %v, want the unavailable sentinel", err)
	}
}

// TestFetchFileRefusesEveryNonRegularShape pins the mode check made on the
// opened descriptor. Only the named-pipe row depends on it; the directory and
// absent rows are refused by other routes and stay to name the shapes.
func TestFetchFileRefusesEveryNonRegularShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		build  func(t *testing.T, dir string) string
		name   string
		wantOK bool
	}{
		{name: "named pipe", build: buildNamedPipeSource},
		{name: "directory", build: buildDirectorySource},
		{name: "absent path", build: buildAbsentSource},
		{name: "positive control: regular file", build: buildRegularSource, wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			source := "file://" + tc.build(t, t.TempDir())

			// Without O_NONBLOCK the named-pipe open blocks for a writer and
			// hangs; without the mode check a writerless FIFO reads as empty.
			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), source)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", source, err)
				}

				return
			}
			if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
				t.Fatalf("%s: error = %v, want the unavailable sentinel", tc.name, err)
			}
		})
	}
}

// TestReadFileReportsAReadFailure drives readFile directly, since fetchFile
// refuses non-regular files first: a directory is the read failure every
// platform can stage, and a regular file is the positive control.
func TestReadFileReportsAReadFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fetcher := newTestFetcher(false, helpers.SignatureMaxSize)

	regular, err := os.Open(writeSourceFile(t, dir, sigLeafName, "signature bytes"))
	if err != nil {
		t.Fatalf("open the control file: %v", err)
	}
	defer func() { _ = regular.Close() }()

	data, err := fetcher.readFile(regular, 0)
	if err != nil || string(data) != "signature bytes" {
		t.Fatalf("positive control: readFile(a regular file) = %q, %v, want the file's bytes and nil", data, err)
	}

	// #nosec G304 -- dir is this test's own t.TempDir; opening it is the whole
	// point of the row, since a directory is the read failure a test can stage.
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open the directory: %v", err)
	}
	defer func() { _ = directory.Close() }()

	// A readFile that dropped ReadFrom's error would report this source as read
	// and empty, with the failure gone.
	if data, err = fetcher.readFile(directory, 0); err == nil {
		t.Fatalf("readFile(a directory) = %q, %v, want a read failure", data, err)
	}
}

// TestFetchFileFailuresRenderOneMessage pins that an absent, unreadable,
// non-regular or oversized file source renders one message naming only the
// path, so a repository cannot use it as a filesystem oracle.
func TestFetchFileFailuresRenderOneMessage(t *testing.T) {
	t.Parallel()

	// The control runs at the smaller of the two ceilings, on a body that
	// exactly fills it: the harness is shown capable of acceptance, and the
	// ceiling is shown not to be what refuses the rows below.
	control := writeSourceFile(t, t.TempDir(), sigLeafName, strings.Repeat("a", tinySourceLimit))
	if _, err := newTestFetcher(false, tinySourceLimit).
		FetchRequirementSource(t.Context(), "file://"+control); err != nil {
		t.Fatalf("positive control: FetchRequirementSource(a regular file at the ceiling) error = %v, want nil", err)
	}

	cases := []struct {
		build func(t *testing.T, dir string) string
		name  string
		limit int64
	}{
		{name: "absent path", build: buildAbsentSource, limit: helpers.SignatureMaxSize},
		{name: "unreadable regular file", build: buildUnreadableSource, limit: helpers.SignatureMaxSize},
		{name: "directory", build: buildDirectorySource, limit: helpers.SignatureMaxSize},
		{name: "over the ceiling", build: buildOversizeSource, limit: tinySourceLimit},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := tc.build(t, t.TempDir())

			// Messages go to the log: they carry an OS-chosen temporary path.
			_, err := newTestFetcher(false, tc.limit).FetchRequirementSource(t.Context(), "file://"+path)
			if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
				t.Fatalf("FetchRequirementSource(%q) error = %v, want the unavailable sentinel", path, err)
			}
			if got, want := err.Error(), unavailablePrefix+"file://"+path+unreadableSuffix; got != want {
				t.Logf("message = %q, want %q", got, want)
				t.Fatalf("%s renders a message of its own", tc.name)
			}
		})
	}
}

// buildNamedPipeSource makes dir/sig.asc a FIFO, skipping where the platform
// has none. It mirrors cleanup's own named-pipe fixture, which states the same
// blocking-open property this row exists for.
func buildNamedPipeSource(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, sigLeafName)
	if err := syscall.Mkfifo(path, helpers.FileMod); err != nil {
		t.Skipf("named pipes unavailable on this platform: %v", err)
	}

	return path
}

// buildDirectorySource makes dir/sig.asc a directory.
func buildDirectorySource(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, sigLeafName)
	if err := os.Mkdir(path, helpers.DirMod); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}

	return path
}

// buildAbsentSource names a leaf dir deliberately does not hold.
func buildAbsentSource(_ *testing.T, dir string) string {
	return filepath.Join(dir, sigLeafName)
}

// buildUnreadableSource writes a signature file with every permission removed,
// skipping where the mode does not bite (root, or a platform without it),
// since there the fixture cannot produce an unreadable file.
func buildUnreadableSource(t *testing.T, dir string) string {
	t.Helper()

	path := writeSourceFile(t, dir, sigLeafName, sourceBlobBody)
	if err := os.Chmod(path, 0); err != nil {
		t.Skipf("chmod unavailable on this platform: %v", err)
	}
	if probe, err := os.Open(path); err == nil { //nolint:gosec // G304: the path is this test's own fixture
		_ = probe.Close()
		t.Skip("this principal reads a mode-0 file, so the fixture cannot produce an unreadable regular file")
	}

	return path
}

// buildOversizeSource writes one byte more than tinySourceLimit, the ceiling
// its row runs the fetcher at.
func buildOversizeSource(t *testing.T, dir string) string {
	t.Helper()

	return writeSourceFile(t, dir, sigLeafName, strings.Repeat("a", tinySourceLimit+1))
}

// buildRegularSource writes a genuine signature file, the shape every row above
// is contrasted against.
func buildRegularSource(t *testing.T, dir string) string {
	t.Helper()

	return writeSourceFile(t, dir, sigLeafName, sourceBlobBody)
}

// TestFetchFileURLUnderOffline pins that offline is about the network: a local
// file source is still read. TestFetchHTTPURLUnderOfflineIsRefused is its pair.
func TestFetchFileURLUnderOffline(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)

	blob, err := newTestFetcher(true, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), "file://"+path)
	if err != nil {
		t.Fatalf("FetchRequirementSource(a file url, offline) error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("data = %q, want the fixture blob", blob.Data)
	}
}

// TestFetchHTTPURLUnderOfflineIsRefused pins that offline refuses an http
// source before a request is composed, so the message omits the capability
// query; the same URL fetched online is the positive control.
func TestFetchHTTPURLUnderOfflineIsRefused(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	source := srv.URL + "/" + sigLeafName + presignedQuery

	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), source)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(the same url, online) error = %v, want nil", err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	// The offline transport raises the same two sentinels, so only the message
	// assertion shows the refusal came from fetchHTTP's early return.
	_, err = newTestFetcher(true, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), source)
	if !errors.Is(err, helpers.ErrOfflineMode) {
		t.Fatalf("FetchRequirementSource(an http url, offline) error = %v, want the offline sentinel", err)
	}
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource(an http url, offline) error = %v, want the unavailable sentinel too", err)
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Logf("message = %q", err)
		t.Fatalf("the offline refusal carries the capability query")
	}
}

// TestFetchHTTPSSurfacesCertificateVerification pins the https arm and that
// the fetcher trusts no self-signed certificate: its client holds no relaxed
// TLS policy for any origin.
func TestFetchHTTPSSurfacesCertificateVerification(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sourceBlobBody))
	}))
	t.Cleanup(srv.Close)

	_, err := newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(t.Context(), srv.URL+"/"+sigLeafName)
	if errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
		t.Fatalf("https url refused as unfetchable: %v", err)
	}
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource(an https url with a self-signed certificate) error = %v, want the unavailable sentinel", err)
	}
	if !strings.Contains(err.Error(), "x509") {
		t.Fatalf("error = %v, want an x509 verification failure: this client must trust no self-signed certificate", err)
	}
}

// TestFetchHTTPSurfacesContextCancellation pins that a caller's cancellation
// stays reachable through errors.Is, so exitcode reports a Ctrl-C as an
// interrupt rather than a network failure.
func TestFetchHTTPSurfacesContextCancellation(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
	}))
	// LIFO: the handler is released before Close waits for it, so the fixture
	// cannot deadlock its own teardown.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	_, err := newTestFetcher(false, helpers.SignatureMaxSize).
		FetchRequirementSource(ctx, srv.URL+"/"+sigLeafName)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("FetchRequirementSource(canceled mid-request) error = %v, want context.Canceled reachable", err)
	}
	if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
		t.Fatalf("FetchRequirementSource(canceled mid-request) error = %v, want the unavailable sentinel too", err)
	}
}

// TestTransportCause covers the unwrap directly: http.Client always wraps a
// cause in a *url.Error, so the other shapes are reachable only from here.
func TestTransportCause(t *testing.T) {
	t.Parallel()

	// A real sentinel rather than a fresh errors.New: what the caller wraps has
	// to stay matchable through errors.Is, and helpers.ErrOfflineMode is one of
	// the values cmd/go-galaxy/exitcode actually classifies on.
	inner := helpers.ErrOfflineMode
	cases := []struct {
		err  error
		want error
		name string
	}{
		{name: "not a url.Error", err: inner, want: inner},
		{name: "url.Error carrying no cause", err: &url.Error{Op: "Get", URL: "https://h/x?t=1"}, want: nil},
		{name: "url.Error carrying a cause", err: &url.Error{Op: "Get", URL: "https://h/x?t=1", Err: inner}, want: inner},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := transportCause(tc.err)
			want := tc.want
			if want == nil {
				want = tc.err
			}
			if !errors.Is(got, want) {
				t.Fatalf("transportCause(%v) = %v, want %v", tc.err, got, want)
			}
		})
	}
}

// TestFetchRequirementSourceRefusesAHostlessHTTPURL pins that an http(s) URL
// naming no host is refused as an unsupported source and exits 2; left to
// http.Client it would exit 4, the network class a CI retries.
func TestFetchRequirementSourceRefusesAHostlessHTTPURL(t *testing.T) {
	t.Parallel()

	srv, _ := newSourceServer(t, sourceBlobBody)
	control := srv.URL + "/" + sigLeafName
	blob, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), control)
	if err != nil {
		t.Fatalf("positive control: FetchRequirementSource(%q) error = %v, want nil", control, err)
	}
	if string(blob.Data) != sourceBlobBody {
		t.Fatalf("positive control: data = %q, want the fixture blob", blob.Data)
	}

	cases := []struct {
		name   string
		source string
	}{
		{name: "https with an empty authority and a path", source: "https:///" + sigLeafName},
		{name: "https with nothing after the slashes", source: "https://"},
		{name: "http with nothing after the slashes", source: "http://"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := newTestFetcher(false, helpers.SignatureMaxSize).FetchRequirementSource(t.Context(), tc.source)
			if !errors.Is(err, helpers.ErrUnsupportedSignatureSource) {
				t.Logf("error = %v", err)
				t.Fatalf("%q was not refused as an unsupported source", tc.source)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitUsage {
				t.Fatalf("%q classified as exit %d, want ExitUsage (%d)", tc.source, got, exitcode.ExitUsage)
			}
		})
	}
}

// TestFetchFileNamesThePathItOpened pins that a fragment is cut from a file
// source's display, since url.Parse drops it before the open: Blob.Origin
// and the failure message must name the path actually opened.
func TestFetchFileNamesThePathItOpened(t *testing.T) {
	t.Parallel()

	// The leaf a fragment-carrying source spells out in full, and the part of
	// it url.Parse keeps as the Path: everything from the "#" on is a fragment
	// that never reaches the open.
	const (
		baseLeaf = "a"
		fullLeaf = baseLeaf + "#b.asc"
		// decoyBody sits at fullLeaf, so a read that reached that file rather
		// than baseLeaf fails on the bytes before it fails on the name.
		decoyBody = "decoy-body-that-must-not-be-read"
	)

	t.Run("the file that is opened", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		opened := writeSourceFile(t, dir, baseLeaf, sourceBlobBody)
		writeSourceFile(t, dir, fullLeaf, decoyBody)

		blob, err := newTestFetcher(false, helpers.SignatureMaxSize).
			FetchRequirementSource(t.Context(), "file://"+filepath.Join(dir, fullLeaf))
		if err != nil {
			t.Fatalf("FetchRequirementSource(a file url carrying a fragment) error = %v, want nil", err)
		}
		if string(blob.Data) != sourceBlobBody {
			t.Fatalf("data = %q, want the bytes of the file named before the fragment", blob.Data)
		}

		// The rendered values go to the log: each carries an OS-chosen path.
		if blob.Origin != "file://"+opened {
			t.Logf("origin = %q, want %q", blob.Origin, "file://"+opened)
			t.Fatalf("Blob.Origin names a path other than the one that was opened")
		}
	})

	t.Run("the message that names it", func(t *testing.T) {
		t.Parallel()

		absent := filepath.Join(t.TempDir(), baseLeaf)

		// The same property as above, asserted through the failure message.
		_, err := newTestFetcher(false, helpers.SignatureMaxSize).
			FetchRequirementSource(t.Context(), "file://"+absent+"#b.asc")
		if !errors.Is(err, helpers.ErrSignatureSourceUnavailable) {
			t.Fatalf("FetchRequirementSource(an absent path carrying a fragment) error = %v, want the unavailable sentinel", err)
		}
		if got, want := err.Error(), unavailablePrefix+"file://"+absent+unreadableSuffix; got != want {
			t.Logf("message = %q, want %q", got, want)
			t.Fatalf("the message names a path other than the one that was opened")
		}
	})
}

// TestSourceRequiresNetwork pins the predicate on its sibling tables' samples
// plus an https row: only http and https need the network, and no file
// source does, the LocalHost spelling included.
func TestSourceRequiresNetwork(t *testing.T) {
	t.Parallel()

	path := writeSourceFile(t, t.TempDir(), sigLeafName, sourceBlobBody)
	srv, _ := newSourceServer(t, sourceBlobBody)

	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{name: "file url with an empty authority", source: "file://" + path, want: false},
		{name: "file url naming localhost", source: "file://localhost" + path, want: false},
		{name: "file url naming LocalHost", source: "file://LocalHost" + path, want: false},
		{name: "file url with a single slash", source: "file:" + path, want: false},
		{name: "http url", source: srv.URL + "/" + sigLeafName, want: true},
		{name: "https url naming a host", source: "https://example.invalid/" + sigLeafName, want: true},
		{name: "ftp scheme", source: "ftp://h/x", want: false},
		{name: "empty", source: "", want: false},
		{name: "file url naming a foreign host", source: "file://evil.example/x", want: false},
		{name: "unparseable url", source: "http://%zz/x", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SourceRequiresNetwork(tc.source); got != tc.want {
				t.Fatalf("SourceRequiresNetwork(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}

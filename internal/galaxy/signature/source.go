package signature

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// fileScheme, httpScheme and httpsScheme are the whole allow-list
	// FetchRequirementSource dispatches on; url.Parse already lowercases a
	// scheme, so they are compared directly.
	fileScheme  = "file"
	httpScheme  = "http"
	httpsScheme = "https"

	// localhostAuthority is the one authority a file URL may name besides the
	// empty one. RFC 8089 gives "this host" both spellings, and a file URL
	// naming any other authority names a file on another machine.
	localhostAuthority = "localhost"
)

// Fetcher gathers signature blobs from the sources a requirements file names.
// It is read-only once built and shared by a run's workers. It builds its own
// client, so a Galaxy token or relaxed TLS never reaches repository content.
type Fetcher struct {
	client *http.Client
	// limit bounds one blob's bytes, on both the file and the http paths.
	limit int64
	// offline lets fetchHTTP refuse before composing a request, so the refusal
	// names the display form rather than the transport's rendering of the URL.
	offline bool
}

// NewFetcher returns a Fetcher capped at helpers.SignatureMaxSize per blob.
// timeout is a no-progress budget (first byte, idle read), not a request
// ceiling; offline refuses http sources while file sources still work.
func NewFetcher(timeout time.Duration, offline bool) *Fetcher {
	return newFetcher(timeout, offline, helpers.SignatureMaxSize)
}

// newFetcher is NewFetcher with the size ceiling as a parameter, so a test can
// cross it without a fixture the size of the real one.
func newFetcher(timeout time.Duration, offline bool, limit int64) *Fetcher {
	return &Fetcher{client: fetch.NewUnauthenticated(timeout, offline), limit: limit, offline: offline}
}

// FetchRequirementSource fetches one source in a single attempt, no retry.
// source must come from a requirements file: a server- or snapshot-supplied
// URI needs a sibling that refuses the file scheme, never this method.
func (f *Fetcher) FetchRequirementSource(ctx context.Context, source string) (Blob, error) {
	parsed, display, err := parseRequirementSource(source)
	if err != nil {
		return Blob{}, err
	}

	// parseRequirementSource admits only three schemes, hence no default arm:
	// one grammar keeps load-time validation and this fetch from disagreeing.
	if parsed.Scheme == fileScheme {
		return f.fetchFile(parsed, display)
	}

	return f.fetchHTTP(ctx, source, display)
}

// ValidateRequirementSource reports, with the fetch's own errors and touching
// nothing, whether FetchRequirementSource could fetch source, so the loader
// refuses a bad source as a usage error naming the file, not a worker failure.
func ValidateRequirementSource(source string) error {
	_, _, err := parseRequirementSource(source)

	return err
}

// SourceRequiresNetwork reports whether FetchRequirementSource would dispatch
// source to fetchHTTP, the arm --offline refuses. A refused source answers
// false; every accepted scheme but file answers true, through the same grammar.
func SourceRequiresNetwork(source string) bool {
	parsed, _, err := parseRequirementSource(source)
	if err != nil {
		return false
	}

	return parsed.Scheme != fileScheme
}

// parseRequirementSource applies the whole signature source grammar and returns
// the parsed URL with its display form (no userinfo, query or fragment), which
// is computed first so even a value url.Parse rejects can be named.
func parseRequirementSource(source string) (*url.URL, string, error) {
	display := helpers.URLForMessage(source)

	parsed, err := url.Parse(source)
	if err != nil {
		// The parse error is deliberately not wrapped in: url.Error renders the
		// whole value it failed on, query string and userinfo included.
		return nil, display, fmt.Errorf("%w: %q could not be parsed as a URL", helpers.ErrUnsupportedSignatureSource, display)
	}
	if parsed.User != nil {
		return nil, display, fmt.Errorf("%w: %q", helpers.ErrSignatureSourceUserinfo, display)
	}
	// An opaque URL, or an http(s) URL with no host, names nothing fetchable. An
	// http(s) one left to the switch would fail in http.Client as an unreachable
	// source, the failure class a CI is most likely to retry forever.
	if parsed.Opaque != "" || hostlessHTTP(parsed) {
		return nil, display, fmt.Errorf("%w: %q names no host and no absolute path",
			helpers.ErrUnsupportedSignatureSource, display)
	}
	switch parsed.Scheme {
	case fileScheme:
		return parsed, display, checkFileSource(parsed, display)
	case httpScheme, httpsScheme:
		return parsed, display, nil
	default:
		// The empty scheme lands here too: unlike a collection's source:, a bare
		// server_list id resolves to nothing for a signature source.
		return nil, display, fmt.Errorf("%w: %q", helpers.ErrUnsupportedSignatureSource, display)
	}
}

// checkFileSource applies the file sub-grammar at load time: an empty or
// localhost authority and an absolute path. Refused only in fetchFile, such a
// value would exit as an install failure instead of a usage error.
func checkFileSource(u *url.URL, display string) error {
	if u.Host != "" && !strings.EqualFold(u.Host, localhostAuthority) {
		return fmt.Errorf("%w: %q names a host this tool cannot read a file from",
			helpers.ErrUnsupportedSignatureSource, display)
	}
	if u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return fmt.Errorf("%w: %q does not name an absolute local path",
			helpers.ErrUnsupportedSignatureSource, display)
	}

	return nil
}

// hostlessHTTP reports whether u is an http or https URL with no authority
// ("https:///sig.asc"), which url.Parse accepts but no request can use. The
// scheme test matters: "file:///abs" names no authority by design.
func hostlessHTTP(u *url.URL) bool {
	return u.Host == "" && (u.Scheme == httpScheme || u.Scheme == httpsScheme)
}

// fetchFile reads a blob from the local path a file URL names. O_NONBLOCK keeps
// a planted FIFO from blocking the open, the mode is checked on the opened
// descriptor, and every read failure renders one message, so none is an oracle.
func (f *Fetcher) fetchFile(u *url.URL, display string) (Blob, error) {
	if u.Host != "" && !strings.EqualFold(u.Host, localhostAuthority) {
		return Blob{}, fmt.Errorf("%w: %q names a host this tool cannot read a file from",
			helpers.ErrUnsupportedSignatureSource, display)
	}
	// A backstop to checkFileSource: this function's own question is whether
	// the URL names an absolute local path.
	if u.Opaque != "" || !strings.HasPrefix(u.Path, "/") {
		return Blob{}, fmt.Errorf("%w: %q does not name an absolute local path",
			helpers.ErrUnsupportedSignatureSource, display)
	}

	// #nosec G304,G703 -- u.Path comes from a requirements file, which is
	// repository content; reading the local path it names is the entire
	// operation, and the residual that accepts is disclosed in docs/security.md.
	file, err := os.OpenFile(u.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Blob{}, unreadableFileSource(display)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Blob{}, unreadableFileSource(display)
	}

	data, err := f.readFile(file, info.Size())
	if err != nil || int64(len(data)) > f.limit {
		return Blob{}, unreadableFileSource(display)
	}

	return Blob{Origin: display, Data: data}, nil
}

// unreadableFileSource is the one error every file-path failure returns. It
// wraps no OS error, so absent, unreadable, non-regular and oversized are one
// answer, and no caller can branch on fs.ErrNotExist for a signature source.
func unreadableFileSource(display string) error {
	return fmt.Errorf("%w: %q could not be read", helpers.ErrSignatureSourceUnavailable, display)
}

// readFile reads at most limit+1 bytes so the caller can tell a file of exactly
// limit bytes from a longer one. The +1 must not reach the http path, whose
// size-limited reader already fails on the first byte past the ceiling.
func (f *Fetcher) readFile(file *os.File, size int64) ([]byte, error) {
	// The extra bytes.MinRead is the headroom ReadFrom wants available before
	// it stops, so an ordinary signature is read into one allocation.
	buf := bytes.NewBuffer(make([]byte, 0, min(max(size, 0), f.limit)+bytes.MinRead))
	if _, err := buf.ReadFrom(&io.LimitedReader{R: file, N: f.limit + 1}); err != nil {
		// A vanished mount or medium error is reported as an unavailable
		// source, never as a truncated blob the signature check would judge.
		return nil, err
	}

	return buf.Bytes(), nil
}

// fetchHTTP fetches a blob over http or https in one attempt; a non-200 is
// named by its status code alone and its body is never read. Redirects are
// followed and no address class is refused, which docs/security.md discloses.
func (f *Fetcher) fetchHTTP(ctx context.Context, source, display string) (Blob, error) {
	if f.offline {
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, helpers.ErrOfflineMode)
	}

	// The request carries the full source, query string included: the query may
	// be the capability that makes the source fetchable at all. Only what is
	// reported back is stripped.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		// Unreachable, as the URL already parsed. transportCause, not err: a
		// *url.Error from url.Parse renders the raw value, query and userinfo
		// included.
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, transportCause(err))
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, transportCause(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Blob{}, fmt.Errorf("%w: %q: HTTP %d", helpers.ErrSignatureSourceUnavailable, display, resp.StatusCode)
	}

	// Not pre-sized from Content-Length: whoever answered chose that header, and
	// sizing from it would let a source spend this process's memory for free.
	data, err := io.ReadAll(helpers.NewSizeLimitedReader(resp.Body, f.limit))
	if err != nil {
		return Blob{}, fmt.Errorf("%w: %q: %w", helpers.ErrSignatureSourceUnavailable, display, err)
	}

	return Blob{Origin: display, Data: data}, nil
}

// transportCause returns the error inside a *url.Error, whose rendering carries
// the whole request URL, query included. Callers wrap the inner error with %w,
// so errors.Is still reaches the causes cmd/go-galaxy/exitcode classifies on.
func transportCause(err error) error {
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok || urlErr.Err == nil {
		return err
	}

	return urlErr.Err
}

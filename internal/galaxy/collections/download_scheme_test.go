package collections

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// nonHTTPDownloadURL is the refused download_url both validateDownloadInputs
// tests below drive: a file: URL is the shape a poisoned snapshot would take
// to aim an artifact fetch at the local filesystem instead of at a server.
const nonHTTPDownloadURL = "file:///etc/passwd"

// userinfoDownloadURL is the refused download_url the userinfo tests drive: a
// perfectly fetchable https URL carrying a credential a server, not the
// operator, chose.
// #nosec G101 -- test fixture literal, not a real credential
const userinfoDownloadURL = "https://u:p@h/a.tar.gz"

// checkDownloadURLCase is one table entry for TestCheckDownloadURL. wantErr is
// the sentinel the case must carry, or nil for a URL that must be accepted.
type checkDownloadURLCase struct {
	wantErr error
	name    string
	raw     string
}

// checkDownloadURLCases pins that only an absolute http(s) URL with a host and
// no userinfo is accepted. The scheme is judged before userinfo, so the
// userinfo sentinel only ever names an otherwise fetchable URL.
func checkDownloadURLCases() []checkDownloadURLCase {
	return []checkDownloadURLCase{
		{name: "https accepted", raw: "https://h/a.tar.gz", wantErr: nil},
		{name: "http accepted", raw: "http://h/a.tar.gz", wantErr: nil},
		{name: "scheme case is not significant", raw: "HTTPS://H/a.tar.gz", wantErr: nil},
		{name: "at sign in the path accepted", raw: "https://h/x@y.tar.gz", wantErr: nil},
		{name: "file scheme refused", raw: nonHTTPDownloadURL, wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{name: "ftp scheme refused", raw: "ftp://h/a", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		// #nosec G101 -- test fixture literal, not a real credential
		{name: "ftp scheme refused with userinfo", raw: "ftp://u:p@h/x", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{name: "relative reference refused", raw: "/local/a.tar.gz", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{name: "empty host refused", raw: "https:///a", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{
			// A raw control character makes url.Parse fail outright - the same
			// input shape offServerHostGuardCases uses for its own unparseable
			// rows.
			name: "unparseable url refused", raw: "http://exa\x7fmple.com", wantErr: helpers.ErrUnsupportedDownloadURLScheme,
		},
		{name: "userinfo pair refused", raw: userinfoDownloadURL, wantErr: helpers.ErrDownloadURLUserinfo},
		{name: "bare username refused", raw: "https://u@h/a.tar.gz", wantErr: helpers.ErrDownloadURLUserinfo},
	}
}

// TestCheckDownloadURL drives the check directly over every shape in
// checkDownloadURLCases.
func TestCheckDownloadURL(t *testing.T) {
	t.Parallel()

	for _, tt := range checkDownloadURLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkDownloadURL(tt.raw)
			if tt.wantErr == nil {
				if got != nil {
					t.Fatalf("checkDownloadURL(%q) = %v, want nil", tt.raw, got)
				}
				return
			}
			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("checkDownloadURL(%q) = %v, want %v", tt.raw, got, tt.wantErr)
			}
		})
	}
}

// TestCheckDownloadURLDoesNotEchoCredential pins that the refusal names the
// URL's origin and path without its userinfo or query; the checks are
// independent Errorf calls so a leak and a missing name each surface.
func TestCheckDownloadURLDoesNotEchoCredential(t *testing.T) {
	t.Parallel()

	// #nosec G101 -- test fixture literal, not a real credential
	const raw = "https://u:sup3rsecret@h/a.tar.gz?X-Amz-Signature=deadbeef#frag"
	err := checkDownloadURL(raw)
	if err == nil {
		t.Fatal("checkDownloadURL accepted a userinfo-bearing URL, want a refusal")
	}
	msg := err.Error()

	if strings.Contains(msg, "sup3rsecret") {
		t.Errorf("refusal message contains the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("refusal message contains the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("refusal message contains the presigned query: %s", msg)
	}
	if !strings.Contains(msg, "https://h/a.tar.gz") {
		t.Errorf("refusal message does not name the refused URL's origin and path: %s", msg)
	}
}

// downloadInputsFixture builds the cfg and artifact store validateDownloadInputs
// needs to get past its own nil checks, leaving meta.DownloadURL as the only
// input left for it to judge.
func downloadInputsFixture(t *testing.T) (*config.Config, cacheManager.ArtifactStore) {
	t.Helper()
	cacheDir := t.TempDir()
	return &config.Config{CacheDir: cacheDir}, local.NewArtifacts(cacheDir)
}

// TestValidateDownloadInputsRejectsNonHTTPScheme pins that
// validateDownloadInputs itself refuses a non-http download URL, before any
// request, with helpers.ErrUnsupportedDownloadURLScheme.
func TestValidateDownloadInputsRejectsNonHTTPScheme(t *testing.T) {
	t.Parallel()
	cfg, artifacts := downloadInputsFixture(t)

	err := validateDownloadInputs(cfg, artifacts, &types.GalaxyCollectionVersionInfo{DownloadURL: nonHTTPDownloadURL})

	if !errors.Is(err, helpers.ErrUnsupportedDownloadURLScheme) {
		t.Fatalf("validateDownloadInputs(%q) = %v, want helpers.ErrUnsupportedDownloadURLScheme", nonHTTPDownloadURL, err)
	}
}

// TestValidateDownloadInputsRejectsUserinfo pins that validateDownloadInputs
// refuses a userinfo URL with helpers.ErrDownloadURLUserinfo, and accepts the
// same URL with only the credential removed.
func TestValidateDownloadInputsRejectsUserinfo(t *testing.T) {
	t.Parallel()
	cfg, artifacts := downloadInputsFixture(t)

	err := validateDownloadInputs(cfg, artifacts, &types.GalaxyCollectionVersionInfo{DownloadURL: userinfoDownloadURL})
	if !errors.Is(err, helpers.ErrDownloadURLUserinfo) {
		t.Fatalf("validateDownloadInputs(%q) = %v, want helpers.ErrDownloadURLUserinfo", userinfoDownloadURL, err)
	}

	const cleanDownloadURL = "https://h/a.tar.gz"
	if err := validateDownloadInputs(cfg, artifacts, &types.GalaxyCollectionVersionInfo{DownloadURL: cleanDownloadURL}); err != nil {
		t.Fatalf("control: validateDownloadInputs(%q) = %v, want nil", cleanDownloadURL, err)
	}
}

// TestValidateDownloadInputsAcceptsHTTPS is the positive control for
// TestValidateDownloadInputsRejectsNonHTTPScheme: the same fixture with an
// https download URL passes.
func TestValidateDownloadInputsAcceptsHTTPS(t *testing.T) {
	t.Parallel()
	cfg, artifacts := downloadInputsFixture(t)

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: "https://galaxy.example.invalid/a.tar.gz"}

	if err := validateDownloadInputs(cfg, artifacts, meta); err != nil {
		t.Fatalf("validateDownloadInputs with an https download url = %v, want nil", err)
	}
}

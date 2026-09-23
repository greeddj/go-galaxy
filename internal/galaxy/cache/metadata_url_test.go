package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// metadataFixturePassword is the credential every fixture here smuggles into
// a server-supplied metadata URL, distinctive so a substring search for it in
// a rendered message is an answer rather than a coincidence.
const metadataFixturePassword = "pa55w0rd-must-not-be-rendered"

// metadataFixtureHostPath is the part of every fixture URL an operator reading
// a failure actually needs, and the part no rule here cuts.
const metadataFixtureHostPath = "hub.example/api/v3/collections/acme/widgets/versions/"

// metadataFixtureURL is that host and path with a scheme and nothing else: the
// form a cut message must still name, and the control fixture's whole URL.
const metadataFixtureURL = "https://" + metadataFixtureHostPath

// metadataFixtureUserinfoURL is the same value carrying a credential in its
// authority, which is what a message must not name.
const metadataFixtureUserinfoURL = "https://u:" + metadataFixturePassword + "@" + metadataFixtureHostPath

// metadataFixtureQuery is the capability half of the same fixture: a presigned
// query is what a message naming a URL must drop even where the URL itself is
// worth naming.
const metadataFixtureQuery = "?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900"

// metadataUnbuildableURL carries the fixture credential beside a port
// url.Parse refuses: the value helpers.ErrMetadataRequestBuildFailed exists
// for, and one collections.checkMetadataURLUserinfo passes through unjudged.
const metadataUnbuildableURL = "https://u:" + metadataFixturePassword + "@hub.example:notaport/api/v3/collections/"

// notFoundClient returns a client answering every request with a bare 404, so
// a fixture reaches fetchJSONBodyOnce's status arm without a server: these
// fixtures name a host that resolves nowhere, on purpose.
func notFoundClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})}
}

// okJSONClient returns a client answering every request with 200 and a minimal
// JSON document, the shape a request that actually reaches an endpoint gets
// back.
func okJSONClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
		}, nil
	})}
}

// TestHTTPStatusErrorNamesTheURLWithItsCredentialsCut pins, through the
// rendered message, that a status error names the URL with userinfo and query
// cut; independent t.Errorf checks report each defect separately.
func TestHTTPStatusErrorNamesTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	raw := metadataFixtureUserinfoURL + metadataFixtureQuery
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), notFoundClient(), raw, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("a 404 returned no error, so this fixture never reached the status arm")
	}
	msg := err.Error()

	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("status error carries the presigned query: %s", msg)
	}
	if strings.Contains(msg, metadataFixturePassword) {
		t.Errorf("status error carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("status error carries the userinfo prefix %q: %s", "u:", msg)
	}
	if !strings.Contains(msg, metadataFixtureURL) || !strings.Contains(msg, "404") {
		t.Errorf("status error does not name the URL's scheme, host and path alongside its status: %s", msg)
	}

	assertStatusErrorNamesTheCleanURL(t)
}

// assertStatusErrorNamesTheCleanURL is the control described on
// TestHTTPStatusErrorNamesTheURLWithItsCredentialsCut: the same fixture with
// nothing to cut must render its URL intact.
func assertStatusErrorNamesTheCleanURL(t *testing.T) {
	t.Helper()

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), notFoundClient(), metadataFixtureURL, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("control: a 404 returned no error")
	}
	if !strings.Contains(err.Error(), metadataFixtureURL) {
		t.Errorf("control: a status error over a credential-free URL does not name it: %s", err.Error())
	}
}

// TestFetchJSONBodyRefusesAURLNoRequestCanBeBuiltFrom pins that a URL
// url.Parse rejects is reported as helpers.ErrMetadataRequestBuildFailed,
// naming no part of it: net/http's *url.Error would print the password.
func TestFetchJSONBodyRefusesAURLNoRequestCanBeBuiltFrom(t *testing.T) {
	t.Parallel()

	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), okJSONClient(), metadataUnbuildableURL, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("a URL url.Parse refuses was accepted, want a refusal")
	}
	msg := err.Error()

	if strings.Contains(msg, metadataFixturePassword) {
		t.Errorf("refusal message carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("refusal message carries the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, metadataUnbuildableURL) {
		t.Errorf("refusal message carries the raw value: %s", msg)
	}
	if strings.Contains(msg, "hub.example") {
		t.Errorf("refusal message names part of the raw value: %s", msg)
	}
	if !errors.Is(err, helpers.ErrMetadataRequestBuildFailed) {
		t.Errorf("refusal does not classify as helpers.ErrMetadataRequestBuildFailed: %v", err)
	}
	if !strings.Contains(msg, "metadata url") {
		t.Errorf("refusal message names nothing actionable: %s", msg)
	}

	assertBuildableURLReachesTheClient(t)
}

// assertBuildableURLReachesTheClient is the control for
// TestFetchJSONBodyRefusesAURLNoRequestCanBeBuiltFrom: the same fixture with a
// valid port reaches the client, so the refusal is the guard's doing.
func assertBuildableURLReachesTheClient(t *testing.T) {
	t.Helper()

	buildable := strings.Replace(metadataUnbuildableURL, ":notaport", ":8443", 1)
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), okJSONClient(), buildable, nil, &out, Policy{}, 0)
	if err != nil {
		t.Fatalf("control: the same fixture with a real port err = %v, want nil", err)
	}
	if out["ok"] != true {
		t.Fatal("control: the response body did not decode, so the request never reached the client")
	}
}

// dialFailureClient returns a client whose transport fails before any
// response, as a refused dial, DNS or TLS failure does; net/http wraps that in
// a *url.Error carrying the request URL, the value under test.
func dialFailureClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errDialRefusedFixture
	})}
}

// TestMetadataTransportErrorNamesTheURLWithItsCredentialsCut pins
// helpers.CutTransportURL on this path: net/http masks a password but keeps
// the query, which on a server-chosen URL is itself the capability.
func TestMetadataTransportErrorNamesTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	raw := metadataFixtureUserinfoURL + metadataFixtureQuery
	var out map[string]any
	err := FetchJSONWithCachePolicy(context.Background(), dialFailureClient(), raw, nil, &out, Policy{}, 0)
	if err == nil {
		t.Fatal("a failing transport returned no error, so this fixture never reached the transport arm")
	}
	msg := err.Error()

	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("transport error carries the presigned query: %s", msg)
	}
	if strings.Contains(msg, metadataFixturePassword) {
		t.Errorf("transport error carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("transport error carries the userinfo prefix %q: %s", "u:", msg)
	}
	if !strings.Contains(msg, metadataFixtureHostPath) {
		t.Errorf("transport error does not name the host and path it failed to reach: %s", msg)
	}

	assertTransportErrorStaysClassifiable(t, err)
}

// assertTransportErrorStaysClassifiable pins that the cut leaves the original
// *url.Error reachable through errors.As, since fetchRetryable and
// deadlineError classify through Unwrap.
func assertTransportErrorStaysClassifiable(t *testing.T, err error) {
	t.Helper()

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("errors.As(err, &*url.Error) failed on %v, want success", err)
	}
	// Equality with the unwrapped verdict, not a fixed verdict: what this
	// wrapper must not do is CHANGE a classification, and asserting a
	// particular answer would pin fetchRetryable's own policy here instead.
	if got, want := fetchRetryable(err), fetchRetryable(urlErr); got != want {
		t.Errorf("fetchRetryable(wrapped) = %v, fetchRetryable(unwrapped) = %v, want them equal", got, want)
	}
}

// errDialRefusedFixture is the transport failure dialFailureClient returns.
var errDialRefusedFixture = errors.New("connect: connection refused")

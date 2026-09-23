package helpers

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// transportFixturePassword is the credential the fixture below smuggles into
// the request URL, distinctive so a substring search cannot match by accident.
const transportFixturePassword = "pa55w0rd-must-not-be-rendered"

// transportFixtureHostPath is the part of that fixture an operator reading the
// failure actually needs, and the part no cut here removes.
const transportFixtureHostPath = "objects.example/acme-widgets-1.0.0.tar.gz"

// transportFixtureURL carries both credential-bearing parts of a URL at once,
// so one fixture covers both halves of the cut under test.
const transportFixtureURL = "https://u:" + transportFixturePassword + "@" + transportFixtureHostPath +
	"?X-Amz-Signature=deadbeefcafe&X-Amz-Expires=900"

// errTransportFixtureCause is the fixture *url.Error's transport failure, a
// sentinel so a check can ask whether the cause is still reachable.
var errTransportFixtureCause = errors.New("connect: connection refused")

// TestCutTransportURLRendersTheURLWithItsCredentialsCut pins that the message
// names host and path without userinfo or query, from a *url.Error spelled as
// net/http leaves it, and that the transport cause stays reachable.
func TestCutTransportURLRendersTheURLWithItsCredentialsCut(t *testing.T) {
	t.Parallel()

	masked := strings.Replace(transportFixtureURL, transportFixturePassword, "***", 1)
	err := CutTransportURL(transportFixtureURL, &url.Error{Op: "Get", URL: masked, Err: errTransportFixtureCause})
	msg := err.Error()

	if strings.Contains(msg, transportFixturePassword) {
		t.Errorf("rendered message carries the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("rendered message carries the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("rendered message carries the presigned query: %s", msg)
	}
	if !strings.Contains(msg, transportFixtureHostPath) {
		t.Errorf("rendered message does not name the host and path it failed to reach: %s", msg)
	}
	if !errors.Is(err, errTransportFixtureCause) {
		t.Errorf("the transport cause is no longer reachable through the wrapper: %v", err)
	}
}

// TestCutTransportURLLeavesANonURLErrorAlone pins that an error that is not a
// *url.Error comes back unchanged, since it never claimed to be about a URL.
func TestCutTransportURLLeavesANonURLErrorAlone(t *testing.T) {
	t.Parallel()

	if got := CutTransportURL(transportFixtureURL, errTransportFixtureCause); !errors.Is(got, errTransportFixtureCause) {
		t.Fatalf("CutTransportURL(plain error) = %v, want it returned unchanged", got)
	}
	// Identity, not just errors.Is: a wrapper that rendered a display around
	// this error would still satisfy the check above.
	if got := CutTransportURL(transportFixtureURL, errTransportFixtureCause); got.Error() != errTransportFixtureCause.Error() {
		t.Fatalf("CutTransportURL(plain error).Error() = %q, want %q", got.Error(), errTransportFixtureCause.Error())
	}
}

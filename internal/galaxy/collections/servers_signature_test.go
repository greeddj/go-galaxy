package collections

import (
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// hubURL and publicURL are two distinct configured server URLs, enough to
// exercise list order and membership without standing a server up.
const (
	hubURL    = "https://hub.example.com/api/automation-hub"
	publicURL = "https://galaxy.ansible.com"
)

// twoServerConfig builds a config whose server list is the given entries, in
// order. Every signature test differs only in that list.
func twoServerConfig(servers ...config.Server) *config.Config {
	cfg := &config.Config{Servers: servers}
	if len(servers) > 0 {
		cfg.Server = servers[0].URL
	}
	return cfg
}

// TestServersSignatureChangesOnReorder pins list order into the signature:
// under first-match ownership a reordered list is a different resolution
// problem whose answer must not be reused.
func TestServersSignatureChangesOnReorder(t *testing.T) {
	t.Parallel()

	forward := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL},
		config.Server{ID: "public", URL: publicURL},
	))
	reversed := serversSignature(twoServerConfig(
		config.Server{ID: "public", URL: publicURL},
		config.Server{ID: "hub", URL: hubURL},
	))

	if forward == reversed {
		t.Fatalf("expected a reordered server list to change the signature, both got %q", forward)
	}
}

// TestServersSignatureIgnoresTokenValue pins that a rotated token leaves the
// signature unchanged: only a token's presence is hashed, because the
// signature is persisted in the snapshot and no token value may land there.
func TestServersSignatureIgnoresTokenValue(t *testing.T) {
	t.Parallel()

	first := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL, Token: config.NewSecret("first-token")},
	))
	rotated := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL, Token: config.NewSecret("second-token-entirely-different")},
	))

	if first != rotated {
		t.Fatalf("expected a rotated token to leave the signature unchanged: %q != %q", first, rotated)
	}
}

// TestServersSignatureChangesWhenTokenAppears pins that a server gaining a
// token changes the signature, since it may reveal collections an anonymous
// read could not see.
func TestServersSignatureChangesWhenTokenAppears(t *testing.T) {
	t.Parallel()

	anonymous := serversSignature(twoServerConfig(config.Server{ID: "hub", URL: hubURL}))
	authenticated := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL, Token: config.NewSecret("t")},
	))

	if anonymous == authenticated {
		t.Fatalf("expected adding a token to change the signature, both got %q", anonymous)
	}
}

// TestServersSignatureChangesOnAppendedServer pins that even a seemingly
// unused added server changes the signature: whether it is unused is not
// knowable without resolving, and the cost is one cold resolve.
func TestServersSignatureChangesOnAppendedServer(t *testing.T) {
	t.Parallel()

	single := serversSignature(twoServerConfig(config.Server{ID: "hub", URL: hubURL}))
	appended := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL},
		config.Server{ID: "public", URL: publicURL},
	))

	if single == appended {
		t.Fatalf("expected an appended server to change the signature, both got %q", single)
	}
}

// TestServersSignatureFallsBackToSingleServer pins that a config with no
// Servers hashes as its one effective cfg.Server, so distinct single servers
// keep distinct signatures.
func TestServersSignatureFallsBackToSingleServer(t *testing.T) {
	t.Parallel()

	legacy := serversSignature(&config.Config{Server: hubURL})
	explicit := serversSignature(twoServerConfig(config.Server{URL: hubURL}))
	if legacy != explicit {
		t.Fatalf("expected a Servers-less config to hash as its single effective server: %q != %q", legacy, explicit)
	}

	other := serversSignature(&config.Config{Server: publicURL})
	if legacy == other {
		t.Fatalf("expected two different single servers to hash differently, both got %q", legacy)
	}

	if got := serversSignature(nil); got != "" {
		t.Fatalf("serversSignature(nil) = %q, want the empty string", got)
	}
}

// TestServersSignatureIsPipeFree pins that the "servers=" header value is hex,
// so it can never carry the "|" separating a per-root line's fields and be
// mistaken for one.
func TestServersSignatureIsPipeFree(t *testing.T) {
	t.Parallel()

	sig := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL + "/weird|path", Token: config.NewSecret("a|b")},
		config.Server{ID: "public", URL: publicURL},
	))

	if strings.Contains(sig, "|") {
		t.Fatalf("serversSignature returned %q, which contains a %q separator", sig, "|")
	}
	if sig == "" {
		t.Fatal("serversSignature returned the empty string for a populated list")
	}
}

// TestRequirementsSignatureFoldsInServerList asserts the header actually
// reaches the requirements signature: the same requirements against a
// different effective server list must not reuse each other's resolution.
func TestRequirementsSignatureFoldsInServerList(t *testing.T) {
	t.Parallel()

	spec := buildRequirementsSpec([]collection{
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
	})

	hubFirst := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL},
		config.Server{ID: "public", URL: publicURL},
	))
	publicFirst := serversSignature(twoServerConfig(
		config.Server{ID: "public", URL: publicURL},
		config.Server{ID: "hub", URL: hubURL},
	))

	withHubFirst := requirementsSignatureFromSpec(spec, false, hubFirst)
	if withHubFirst == requirementsSignatureFromSpec(spec, false, publicFirst) {
		t.Fatal("expected the requirements signature to change with the effective server list")
	}
	if withHubFirst != requirementsSignatureFromSpec(spec, false, hubFirst) {
		t.Fatal("requirements signature is not deterministic across repeated calls")
	}
	// The --no-deps header must stay independently significant: the two
	// fixed-position headers cannot mask one another.
	if withHubFirst == requirementsSignatureFromSpec(spec, true, hubFirst) {
		t.Fatal("expected --no-deps to remain significant alongside the server-list header")
	}
}

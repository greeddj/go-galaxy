package commands

import (
	"errors"
	"reflect"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// TestURLBindingsRevealsPlaintext pins that urlBindings reveals every
// config.URLCredential field into fetch.URLBinding, the origin in helpers.Origin
// form, and that a nil config yields no bindings rather than a panic.
func TestURLBindingsRevealsPlaintext(t *testing.T) {
	t.Parallel()

	prefix, err := urlsource.ParsePrefix("https://artifacts.example/org/")
	if err != nil {
		t.Fatalf("ParsePrefix: %v", err)
	}
	cfg := &config.Config{URLCredentials: []config.URLCredential{
		{ID: "hub", URL: prefix, Token: config.NewSecret("tok-plain")},
	}}

	got := urlBindings(cfg)

	want := []fetch.URLBinding{
		{Origin: "https://artifacts.example:443", PathPrefix: "/org", Token: "tok-plain"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("urlBindings() = %+v, want %+v", got, want)
	}
	if got := urlBindings(nil); got != nil {
		t.Errorf("urlBindings(nil) = %v, want nil", got)
	}
}

// TestURLCredentialsEndToEnd drives a credential from the environment through
// the real install flag set and BuildCollectionConfig to the plain form the
// transport consumes, and pins that a broken binding fails the whole config.
func TestURLCredentialsEndToEnd(t *testing.T) {
	neutralizeAnsibleDiscovery(t)
	t.Setenv("GO_GALAXY_URL_CREDENTIALS", "hub")
	t.Setenv("GO_GALAXY_URL_HUB_URL", "https://artifacts.example/org/")
	t.Setenv("GO_GALAXY_URL_HUB_TOKEN", "tok-plain")

	cfg, err := buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
	if err != nil {
		t.Fatalf("BuildCollectionConfig() error = %v, want nil", err)
	}
	got := urlBindings(cfg)
	if len(got) != 1 || got[0].Origin != "https://artifacts.example:443" ||
		got[0].PathPrefix != "/org" || got[0].Token != "tok-plain" {
		t.Fatalf("urlBindings() = %+v, want the one declared Bearer binding", got)
	}

	t.Setenv("GO_GALAXY_URL_HUB_TOKEN", "")
	_, err = buildConfigFor(t, "install", cliflags.CollectionFlags(), nil)
	if !errors.Is(err, galaxyhelpers.ErrURLCredentialInvalid) {
		t.Fatalf("BuildCollectionConfig() error = %v, want errors.Is helpers.ErrURLCredentialInvalid", err)
	}
}

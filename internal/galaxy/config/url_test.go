package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"go.yaml.in/yaml/v3"
)

// urlTokenPlaintext is the fixture token; distinct from every variable name
// so an error text or a rendering that leaks it is caught by a plain
// Contains.
const urlTokenPlaintext = "ghp-url-token-plaintext" //nolint:gosec // the fixture under test, not a credential

// clearURLEnv removes every GO_GALAXY_URL_* variable from this test's
// environment so a row's verdict is a function of the row alone.
func clearURLEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, urlCredentialEnvPrefix) {
			continue
		}
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", name, err)
		}
	}
}

// setURLEnv clears the surface and then exports env verbatim.
func setURLEnv(t *testing.T, env map[string]string) {
	t.Helper()
	clearURLEnv(t)
	for name, value := range env {
		t.Setenv(name, value)
	}
}

// mustURLPrefix parses a binding the way loadURLCredentials does, for the
// expected values; the raw strings are fixed literals, so a parse failure is
// a fixture bug rather than a case.
func mustURLPrefix(raw string) urlsource.Prefix {
	p, err := urlsource.ParsePrefix(raw)
	if err != nil {
		panic(err)
	}
	return p
}

type urlAcceptedCase struct {
	env  map[string]string
	name string
	want []URLCredential
}

func urlAcceptedCases() []urlAcceptedCase {
	return []urlAcceptedCase{
		{
			name: "https origin",
			env: map[string]string{
				"GO_GALAXY_URL_CREDENTIALS": "hub",
				"GO_GALAXY_URL_HUB_URL":     "https://artifacts.example",
				"GO_GALAXY_URL_HUB_TOKEN":   urlTokenPlaintext,
			},
			want: []URLCredential{{ID: "hub", URL: mustURLPrefix("https://artifacts.example"), Token: NewSecret(urlTokenPlaintext)}},
		},
		{
			name: "path prefix and canonicalized spelling",
			env: map[string]string{
				"GO_GALAXY_URL_CREDENTIALS": "gh",
				"GO_GALAXY_URL_GH_URL":      " https://GitHub.com:443/acme/ ",
				"GO_GALAXY_URL_GH_TOKEN":    urlTokenPlaintext,
			},
			want: []URLCredential{{ID: "gh", URL: mustURLPrefix("https://github.com/acme"), Token: NewSecret(urlTokenPlaintext)}},
		},
		{
			name: "loopback http",
			env: map[string]string{
				"GO_GALAXY_URL_CREDENTIALS": "local",
				"GO_GALAXY_URL_LOCAL_URL":   "http://127.0.0.1:8080",
				"GO_GALAXY_URL_LOCAL_TOKEN": urlTokenPlaintext,
			},
			want: []URLCredential{{ID: "local", URL: mustURLPrefix("http://127.0.0.1:8080"), Token: NewSecret(urlTokenPlaintext)}},
		},
		{
			name: "two ids in list order, dashed id",
			env: map[string]string{
				"GO_GALAXY_URL_CREDENTIALS": "b-hub, a",
				"GO_GALAXY_URL_B-HUB_URL":   "https://b.example",
				"GO_GALAXY_URL_B-HUB_TOKEN": urlTokenPlaintext,
				"GO_GALAXY_URL_A_URL":       "https://a.example",
				"GO_GALAXY_URL_A_TOKEN":     urlTokenPlaintext,
			},
			want: []URLCredential{
				{ID: "b-hub", URL: mustURLPrefix("https://b.example"), Token: NewSecret(urlTokenPlaintext)},
				{ID: "a", URL: mustURLPrefix("https://a.example"), Token: NewSecret(urlTokenPlaintext)},
			},
		},
		{
			name: "token taken verbatim",
			env: map[string]string{
				"GO_GALAXY_URL_CREDENTIALS": "hub",
				"GO_GALAXY_URL_HUB_URL":     "https://artifacts.example",
				"GO_GALAXY_URL_HUB_TOKEN":   urlTokenPlaintext + "\n",
			},
			want: []URLCredential{{ID: "hub", URL: mustURLPrefix("https://artifacts.example"), Token: NewSecret(urlTokenPlaintext + "\n")}},
		},
	}
}

func TestLoadURLCredentialsAccepted(t *testing.T) {
	for _, tc := range urlAcceptedCases() {
		t.Run(tc.name, func(t *testing.T) {
			setURLEnv(t, tc.env)
			cfg := &Config{}
			if err := loadURLCredentials(cfg); err != nil {
				t.Fatalf("loadURLCredentials() error = %v, want nil", err)
			}
			assertURLCredentials(t, cfg.URLCredentials, tc.want)
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", cfg.Warnings)
			}
		})
	}
}

func assertURLCredentials(t *testing.T, got, want []URLCredential) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(URLCredentials) = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.URL != w.URL {
			t.Errorf("URLCredentials[%d] = %+v, want %+v", i, g, w)
		}
		if !g.Token.sameAs(w.Token) {
			t.Errorf("URLCredentials[%d] token differs from the expected plaintext", i)
		}
	}
}

// TestLoadURLCredentialsNone pins that an absent or blank list configures
// nothing: no error, no entries and no warning, even with a stray
// GO_GALAXY_URL_X_* variable for an id that was never declared.
func TestLoadURLCredentialsNone(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":                    {"GO_GALAXY_URL_STRAY_URL": "https://x.example"},
		"blank":                    {"GO_GALAXY_URL_CREDENTIALS": "  "},
		"empty string, stray vars": {"GO_GALAXY_URL_CREDENTIALS": "", "GO_GALAXY_URL_STRAY_TOKEN": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			setURLEnv(t, env)
			cfg := &Config{}
			if err := loadURLCredentials(cfg); err != nil {
				t.Fatalf("loadURLCredentials() error = %v, want nil", err)
			}
			if cfg.URLCredentials != nil || len(cfg.Warnings) != 0 {
				t.Fatalf("URLCredentials = %v, Warnings = %v, want nil and none", cfg.URLCredentials, cfg.Warnings)
			}
		})
	}
}

// bearerHTTPS is the smallest accepted shape, which the refusing rows below
// each break in exactly one way.
func bearerHTTPS(extra map[string]string) map[string]string {
	env := map[string]string{
		"GO_GALAXY_URL_CREDENTIALS": "hub",
		"GO_GALAXY_URL_HUB_URL":     "https://artifacts.example",
		"GO_GALAXY_URL_HUB_TOKEN":   urlTokenPlaintext,
	}
	maps.Copy(env, extra)
	return env
}

type urlRefusedCase struct {
	env         map[string]string
	wantErr     error
	name        string
	wantMention string
}

// urlListRefusedCases are the rows about the id list itself.
func urlListRefusedCases() []urlRefusedCase {
	return []urlRefusedCase{
		{
			name:        "stray comma in the list",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_CREDENTIALS": "hub,,other"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_CREDENTIALS",
		},
		{
			name:        "id outside the alphabet",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_CREDENTIALS": "hub.name"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "hub.name",
		},
		{
			name:        "ids differing only in case",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_CREDENTIALS": "hub,HUB"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_*",
		},
	}
}

// urlBindingRefusedCases are the rows about one id's URL and token.
func urlBindingRefusedCases() []urlRefusedCase {
	return []urlRefusedCase{
		{
			name:        "url unset",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_HUB_URL": ""}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_URL",
		},
		{
			name:        "url unparseable",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_HUB_URL": "artifacts.example"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_URL",
		},
		{
			name:        "ssh binding",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_HUB_URL": "ssh://git.example"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_URL",
		},
		{
			name:        "userinfo in the binding",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_HUB_URL": "https://user@artifacts.example"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_URL",
		},
		{
			name:        "query in the binding",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_HUB_URL": "https://artifacts.example/x?y=1"}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_URL",
		},
		{
			name:        "token unset",
			env:         bearerHTTPS(map[string]string{"GO_GALAXY_URL_HUB_TOKEN": ""}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_HUB_TOKEN",
		},
		{
			name: "token over plaintext http",
			env: bearerHTTPS(map[string]string{
				"GO_GALAXY_URL_HUB_URL": "http://artifacts.example",
			}),
			wantErr:     helpers.ErrInsecureTokenTransport,
			wantMention: "http://artifacts.example:80",
		},
		{
			name: "two ids bound to one URL",
			env: bearerHTTPS(map[string]string{
				"GO_GALAXY_URL_CREDENTIALS":  "hub,mirror",
				"GO_GALAXY_URL_MIRROR_URL":   "https://artifacts.example:443/",
				"GO_GALAXY_URL_MIRROR_TOKEN": urlTokenPlaintext,
			}),
			wantErr:     helpers.ErrURLCredentialInvalid,
			wantMention: "GO_GALAXY_URL_MIRROR_URL",
		},
	}
}

func TestLoadURLCredentialsRefused(t *testing.T) {
	for _, tc := range append(urlListRefusedCases(), urlBindingRefusedCases()...) {
		t.Run(tc.name, func(t *testing.T) {
			setURLEnv(t, tc.env)
			cfg := &Config{}

			err := loadURLCredentials(cfg)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("loadURLCredentials() error = %v, want errors.Is %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error text %q does not mention %q", err.Error(), tc.wantMention)
			}
			if strings.Contains(err.Error(), urlTokenPlaintext) {
				t.Errorf("error text carries the token plaintext: %s", err.Error())
			}
			if cfg.URLCredentials != nil {
				t.Errorf("URLCredentials = %v after a refusal, want nil", cfg.URLCredentials)
			}
		})
	}
}

// TestLoadURLCredentialsUnknownVariableWarns pins one sorted warning per
// unknown variable of a declared id, and none for an undeclared id or for a
// known key of another declared id whose prefix extends this one's.
func TestLoadURLCredentialsUnknownVariableWarns(t *testing.T) {
	setURLEnv(t, map[string]string{
		"GO_GALAXY_URL_CREDENTIALS": "a,a_b",
		"GO_GALAXY_URL_A_URL":       "https://a.example",
		"GO_GALAXY_URL_A_TOKEN":     urlTokenPlaintext,
		"GO_GALAXY_URL_A_B_URL":     "https://ab.example",
		"GO_GALAXY_URL_A_B_TOKEN":   urlTokenPlaintext,
		"GO_GALAXY_URL_A_USERNAME":  "who",   // unknown key of a: url sources take a Bearer token only
		"GO_GALAXY_URL_A_B_SCHEME":  "basic", // unknown key of a_b
		"GO_GALAXY_URL_ZZZ_TOKEN":   "x",     // undeclared id: silently ignored
	})
	cfg := &Config{}
	if err := loadURLCredentials(cfg); err != nil {
		t.Fatalf("loadURLCredentials() error = %v, want nil", err)
	}
	want := []string{
		"unsupported variable GO_GALAXY_URL_A_B_SCHEME ignored",
		"unsupported variable GO_GALAXY_URL_A_USERNAME ignored",
	}
	if len(cfg.Warnings) != len(want) {
		t.Fatalf("Warnings = %v, want %v", cfg.Warnings, want)
	}
	for i := range want {
		if cfg.Warnings[i] != want[i] {
			t.Fatalf("Warnings = %v, want %v (sorted)", cfg.Warnings, want)
		}
	}
}

// TestBrokenGitCredentialWinsOverBrokenURL pins the load order in
// BuildCollectionConfig: a configuration broken in both surfaces reports the
// git failure, so adding the url surface did not change error precedence.
func TestBrokenGitCredentialWinsOverBrokenURL(t *testing.T) {
	setGitEnv(t, map[string]string{
		"GO_GALAXY_GIT_CREDENTIALS": "broken",
	})
	setURLEnv(t, map[string]string{
		"GO_GALAXY_URL_CREDENTIALS": "alsobroken",
	})
	cfg := &Config{}
	err := loadGitCredentials(cfg)
	if err == nil {
		err = loadURLCredentials(cfg)
	}
	if !errors.Is(err, helpers.ErrGitCredentialInvalid) {
		t.Fatalf("error = %v, want the git refusal first", err)
	}
}

func TestURLCredentialRedactsSecrets(t *testing.T) {
	t.Parallel()
	cred := URLCredential{ID: "hub", URL: mustURLPrefix("https://artifacts.example"), Token: NewSecret(urlTokenPlaintext)}

	// Encoding is the subject here, as in the git and S3 tests: musttag
	// wants tags on a struct production never encodes.
	//nolint:musttag // see above
	jsonBytes, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	//nolint:musttag // same fixture, same reason as the json.Marshal above
	yamlBytes, err := yaml.Marshal(cred)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	renderings := map[string]string{
		"%v":   fmt.Sprintf("%v", cred),
		"%+v":  fmt.Sprintf("%+v", cred),
		"%#v":  fmt.Sprintf("%#v", cred),
		"json": string(jsonBytes),
		"yaml": string(yamlBytes),
	}
	for verb, out := range renderings {
		if strings.Contains(out, urlTokenPlaintext) {
			t.Errorf("%s output carries the token plaintext: %s", verb, out)
		}
		if !strings.Contains(out, "artifacts.example") {
			t.Errorf("%s output lost the binding URL: %s", verb, out)
		}
	}
	// The positive control: the plaintext is still reachable where the wire
	// value is built.
	if cred.Token.Reveal() != urlTokenPlaintext {
		t.Fatalf("Reveal() does not return the plaintext the fixture was built with")
	}
}

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"go.yaml.in/yaml/v3"
)

// The plaintexts the fixtures below carry. Each is distinct, and none is a
// substring of a variable name, so an error text or a rendering that leaks
// one is caught by a plain Contains.
const (
	gitPasswordPlaintext   = "hunter2-plaintext"
	gitKeyPlaintext        = "-----BEGIN OPENSSH PRIVATE KEY-----\nkey-material-plaintext\n-----END OPENSSH PRIVATE KEY-----\n"
	gitPassphrasePlaintext = "passphrase-plaintext"
)

// clearGitEnv removes every GO_GALAXY_GIT_* variable from this test's
// environment so a row's verdict is a function of the row alone. t.Setenv is
// called first purely for the restore it registers.
func clearGitEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, gitCredentialEnvPrefix) {
			continue
		}
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v, want nil", name, err)
		}
	}
}

// setGitEnv clears the surface and then exports env verbatim.
func setGitEnv(t *testing.T, env map[string]string) {
	t.Helper()
	clearGitEnv(t)
	for name, value := range env {
		t.Setenv(name, value)
	}
}

// writeKeyFile writes the fixture PEM into a fresh temp dir and returns its
// path, for the rows that bind a key by file.
func writeKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte(gitKeyPlaintext), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v, want nil", path, err)
	}
	return path
}

type gitAcceptedCase struct {
	env  func(t *testing.T) map[string]string
	name string
	want []GitCredential
}

func gitAcceptedCases() []gitAcceptedCase {
	return append(gitAcceptedBasicCases(), gitAcceptedSSHCases()...)
}

func gitAcceptedBasicCases() []gitAcceptedCase {
	return []gitAcceptedCase{
		{
			name: "basic over https",
			env: func(*testing.T) map[string]string {
				return map[string]string{
					"GO_GALAXY_GIT_CREDENTIALS":  "hub",
					"GO_GALAXY_GIT_HUB_URL":      "https://git.example/",
					"GO_GALAXY_GIT_HUB_USERNAME": "ci",
					"GO_GALAXY_GIT_HUB_PASSWORD": gitPasswordPlaintext,
				}
			},
			want: []GitCredential{{
				ID: "hub", URL: mustPrefix("https://git.example"), Username: "ci",
				Password: NewSecret(gitPasswordPlaintext), Kind: GitCredentialBasic,
			}},
		},
		{
			name: "basic over loopback http is tolerated",
			env: func(*testing.T) map[string]string {
				return map[string]string{
					"GO_GALAXY_GIT_CREDENTIALS":    "local",
					"GO_GALAXY_GIT_LOCAL_URL":      "http://127.0.0.1:8080",
					"GO_GALAXY_GIT_LOCAL_USERNAME": "ci",
					"GO_GALAXY_GIT_LOCAL_PASSWORD": gitPasswordPlaintext,
				}
			},
			want: []GitCredential{{
				ID: "local", URL: mustPrefix("http://127.0.0.1:8080"), Username: "ci",
				Password: NewSecret(gitPasswordPlaintext), Kind: GitCredentialBasic,
			}},
		},
	}
}

func gitAcceptedSSHCases() []gitAcceptedCase {
	return []gitAcceptedCase{
		{
			name: "ssh key inline, lower-case id read from the upper-cased variable",
			env: func(*testing.T) map[string]string {
				return map[string]string{
					"GO_GALAXY_GIT_CREDENTIALS":   "myHub",
					"GO_GALAXY_GIT_MYHUB_URL":     "ssh://git.example",
					"GO_GALAXY_GIT_MYHUB_SSH_KEY": gitKeyPlaintext,
				}
			},
			want: []GitCredential{{
				ID: "myHub", URL: mustPrefix("ssh://git.example"),
				SSHKeyPEM: NewSecret(gitKeyPlaintext), Kind: GitCredentialSSHKey,
			}},
		},
		{
			name: "ssh key from a file, with a passphrase and a path prefix kept",
			env: func(t *testing.T) map[string]string {
				t.Helper()
				return map[string]string{
					"GO_GALAXY_GIT_CREDENTIALS":            "org",
					"GO_GALAXY_GIT_ORG_URL":                "ssh://git.example:2222/org/",
					"GO_GALAXY_GIT_ORG_SSH_KEY_FILE":       writeKeyFile(t),
					"GO_GALAXY_GIT_ORG_SSH_KEY_PASSPHRASE": gitPassphrasePlaintext,
				}
			},
			want: []GitCredential{{
				ID: "org", URL: mustPrefix("ssh://git.example:2222/org"),
				SSHKeyPEM: NewSecret(gitKeyPlaintext), SSHPassphrase: NewSecret(gitPassphrasePlaintext),
				Kind: GitCredentialSSHKey,
			}},
		},
		{
			name: "two ids in list order, whitespace around ids trimmed",
			env: func(*testing.T) map[string]string {
				return map[string]string{
					"GO_GALAXY_GIT_CREDENTIALS": " a , b ",
					"GO_GALAXY_GIT_A_URL":       "https://a.example",
					"GO_GALAXY_GIT_A_USERNAME":  "ua",
					"GO_GALAXY_GIT_A_PASSWORD":  gitPasswordPlaintext,
					"GO_GALAXY_GIT_B_URL":       "ssh://b.example",
					"GO_GALAXY_GIT_B_SSH_KEY":   gitKeyPlaintext,
				}
			},
			want: []GitCredential{
				{ID: "a", URL: mustPrefix("https://a.example"), Username: "ua", Password: NewSecret(gitPasswordPlaintext), Kind: GitCredentialBasic},
				{ID: "b", URL: mustPrefix("ssh://b.example"), SSHKeyPEM: NewSecret(gitKeyPlaintext), Kind: GitCredentialSSHKey},
			},
		},
	}
}

// TestLoadGitCredentialsAccepted pins every accepted shape and the exact
// GitCredential each produces, Secrets compared through sameAs since the
// struct is not comparable by ==.
func TestLoadGitCredentialsAccepted(t *testing.T) {
	for _, tc := range gitAcceptedCases() {
		t.Run(tc.name, func(t *testing.T) {
			setGitEnv(t, tc.env(t))
			cfg := &Config{}

			if err := loadGitCredentials(cfg); err != nil {
				t.Fatalf("loadGitCredentials() error = %v, want nil", err)
			}
			assertGitCredentials(t, cfg.GitCredentials, tc.want)
			if len(cfg.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", cfg.Warnings)
			}
		})
	}
}

func assertGitCredentials(t *testing.T, got, want []GitCredential) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(GitCredentials) = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.URL != w.URL || g.Username != w.Username || g.Kind != w.Kind {
			t.Errorf("GitCredentials[%d] = %+v, want %+v", i, g, w)
		}
		if !g.Password.sameAs(w.Password) || !g.SSHKeyPEM.sameAs(w.SSHKeyPEM) || !g.SSHPassphrase.sameAs(w.SSHPassphrase) {
			t.Errorf("GitCredentials[%d] secrets differ from the expected plaintexts", i)
		}
	}
}

// TestLoadGitCredentialsNone pins that an absent or blank list configures
// nothing: no error, no entries and no warning, even beside a stray
// GO_GALAXY_GIT_X_* variable for an id never declared.
func TestLoadGitCredentialsNone(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unset":                    {"GO_GALAXY_GIT_STRAY_URL": "https://x.example"},
		"blank":                    {"GO_GALAXY_GIT_CREDENTIALS": "  "},
		"empty string, stray vars": {"GO_GALAXY_GIT_CREDENTIALS": "", "GO_GALAXY_GIT_STRAY_PASSWORD": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			setGitEnv(t, env)
			cfg := &Config{}
			if err := loadGitCredentials(cfg); err != nil {
				t.Fatalf("loadGitCredentials() error = %v, want nil", err)
			}
			if cfg.GitCredentials != nil || len(cfg.Warnings) != 0 {
				t.Fatalf("GitCredentials = %v, Warnings = %v, want nil and none", cfg.GitCredentials, cfg.Warnings)
			}
		})
	}
}

type gitRefusedCase struct {
	env         func(t *testing.T) map[string]string
	wantErr     error
	name        string
	wantMention string
}

// basicHTTPS is the smallest accepted Basic shape, which the refusing rows
// below each break in exactly one way.
func basicHTTPS(extra map[string]string) func(*testing.T) map[string]string {
	return func(*testing.T) map[string]string {
		env := map[string]string{
			"GO_GALAXY_GIT_CREDENTIALS":  "hub",
			"GO_GALAXY_GIT_HUB_URL":      "https://git.example",
			"GO_GALAXY_GIT_HUB_USERNAME": "ci",
			"GO_GALAXY_GIT_HUB_PASSWORD": gitPasswordPlaintext,
		}
		for k, v := range extra {
			if v == "" {
				delete(env, k)
			} else {
				env[k] = v
			}
		}
		return env
	}
}

// sshInline is basicHTTPS's counterpart for the ssh kind.
func sshInline(extra map[string]string) func(*testing.T) map[string]string {
	return func(*testing.T) map[string]string {
		env := map[string]string{
			"GO_GALAXY_GIT_CREDENTIALS": "hub",
			"GO_GALAXY_GIT_HUB_URL":     "ssh://git.example",
			"GO_GALAXY_GIT_HUB_SSH_KEY": gitKeyPlaintext,
		}
		for k, v := range extra {
			if v == "" {
				delete(env, k)
			} else {
				env[k] = v
			}
		}
		return env
	}
}

func gitRefusedCases() []gitRefusedCase {
	invalid := helpers.ErrGitCredentialInvalid
	return []gitRefusedCase{
		{name: "empty id element", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_CREDENTIALS": "hub,"}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_CREDENTIALS"},
		{name: "id outside the alphabet", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_CREDENTIALS": "my.hub"}),
			wantErr: invalid, wantMention: `"my.hub"`},
		{name: "duplicate id by case", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_CREDENTIALS": "hub,HUB"}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_*"},
		{name: "url missing", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_HUB_URL": ""}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_URL is not set"},
		{name: "url with userinfo is refused by the grammar", env: basicHTTPS(map[string]string{ //nolint:gosec // refusing it is the point
			"GO_GALAXY_GIT_HUB_URL": "https://ci:pw@git.example"}),
			wantErr: helpers.ErrGitURLUserinfo, wantMention: "GO_GALAXY_GIT_HUB_URL"},
		{name: "url in scp form is not a binding", env: sshInline(map[string]string{"GO_GALAXY_GIT_HUB_URL": "git@git.example:org"}),
			wantErr: helpers.ErrInvalidGitURL, wantMention: "GO_GALAXY_GIT_HUB_URL"},
		{name: "password without username", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_HUB_USERNAME": ""}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_PASSWORD is set without GO_GALAXY_GIT_HUB_USERNAME"},
		{name: "username without password", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_HUB_PASSWORD": ""}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_USERNAME is set without GO_GALAXY_GIT_HUB_PASSWORD"},
		{name: "basic fields on an ssh url", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_HUB_URL": "ssh://git.example"}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_USERNAME and GO_GALAXY_GIT_HUB_PASSWORD"},
		{name: "ssh fields on an https url", env: sshInline(map[string]string{"GO_GALAXY_GIT_HUB_URL": "https://git.example"}),
			wantErr: invalid, wantMention: "not GO_GALAXY_GIT_HUB_SSH_KEY"},
		{name: "both key and key file", env: sshInline(map[string]string{"GO_GALAXY_GIT_HUB_SSH_KEY_FILE": "/nonexistent"}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_SSH_KEY and GO_GALAXY_GIT_HUB_SSH_KEY_FILE are both set"},
		{name: "passphrase without a key", env: sshInline(map[string]string{
			"GO_GALAXY_GIT_HUB_SSH_KEY": "", "GO_GALAXY_GIT_HUB_SSH_KEY_PASSPHRASE": gitPassphrasePlaintext}),
			wantErr: invalid, wantMention: "GO_GALAXY_GIT_HUB_SSH_KEY_PASSPHRASE is set without"},
		{name: "ssh url with no key at all", env: sshInline(map[string]string{"GO_GALAXY_GIT_HUB_SSH_KEY": ""}),
			wantErr: invalid, wantMention: "neither GO_GALAXY_GIT_HUB_SSH_KEY nor GO_GALAXY_GIT_HUB_SSH_KEY_FILE"},
		{name: "https url with nothing at all", env: basicHTTPS(map[string]string{
			"GO_GALAXY_GIT_HUB_USERNAME": "", "GO_GALAXY_GIT_HUB_PASSWORD": ""}),
			wantErr: invalid, wantMention: "neither GO_GALAXY_GIT_HUB_USERNAME and GO_GALAXY_GIT_HUB_PASSWORD"},
		{name: "unreadable key file", env: sshInline(map[string]string{
			"GO_GALAXY_GIT_HUB_SSH_KEY": "", "GO_GALAXY_GIT_HUB_SSH_KEY_FILE": "/nonexistent/id_ed25519"}),
			wantErr: os.ErrNotExist, wantMention: "GO_GALAXY_GIT_HUB_SSH_KEY_FILE names a key this process cannot read"},
		{name: "basic over plaintext http off loopback", env: basicHTTPS(map[string]string{"GO_GALAXY_GIT_HUB_URL": "http://git.example"}),
			wantErr: helpers.ErrInsecureTokenTransport, wantMention: `git credential "hub" (http://git.example:80)`},
		{name: "two ids bound to one url", env: func(*testing.T) map[string]string {
			return map[string]string{
				"GO_GALAXY_GIT_CREDENTIALS": "a,b",
				"GO_GALAXY_GIT_A_URL":       "https://git.example/org/",
				"GO_GALAXY_GIT_A_USERNAME":  "ua",
				"GO_GALAXY_GIT_A_PASSWORD":  gitPasswordPlaintext,
				"GO_GALAXY_GIT_B_URL":       "HTTPS://GIT.EXAMPLE:443/org",
				"GO_GALAXY_GIT_B_USERNAME":  "ub",
				"GO_GALAXY_GIT_B_PASSWORD":  gitPasswordPlaintext,
			}
		}, wantErr: invalid, wantMention: "GO_GALAXY_GIT_A_URL and GO_GALAXY_GIT_B_URL bind the same URL"},
	}
}

// TestLoadGitCredentialsRefused drives every refusal, pinning its sentinel, a
// phrase naming the offending variable, and that the text carries no
// password, key or passphrase, so the error is safe to print.
func TestLoadGitCredentialsRefused(t *testing.T) {
	for _, tc := range gitRefusedCases() {
		t.Run(tc.name, func(t *testing.T) {
			setGitEnv(t, tc.env(t))
			cfg := &Config{}

			err := loadGitCredentials(cfg)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("loadGitCredentials() error = %v, want errors.Is %v", err, tc.wantErr)
			}
			if !errors.Is(tc.wantErr, helpers.ErrInsecureTokenTransport) && !errors.Is(err, helpers.ErrGitCredentialInvalid) {
				t.Errorf("loadGitCredentials() error = %v, want errors.Is helpers.ErrGitCredentialInvalid as well", err)
			}
			if !strings.Contains(err.Error(), tc.wantMention) {
				t.Errorf("error text %q does not mention %q", err.Error(), tc.wantMention)
			}
			mustNotContainSecrets(t, err.Error())
			if cfg.GitCredentials != nil {
				t.Errorf("GitCredentials = %v after a refusal, want nil", cfg.GitCredentials)
			}
		})
	}
}

func mustNotContainSecrets(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{gitPasswordPlaintext, "key-material-plaintext", gitPassphrasePlaintext} {
		if strings.Contains(text, secret) {
			t.Errorf("text carries a plaintext secret: %s", text)
		}
	}
}

// TestLoadGitCredentialsUnknownVariableWarns pins one sorted warning per
// unknown variable of a declared id, and none for a known key of another
// declared id whose prefix extends this one's.
func TestLoadGitCredentialsUnknownVariableWarns(t *testing.T) {
	setGitEnv(t, map[string]string{
		"GO_GALAXY_GIT_CREDENTIALS":    "a,a_b",
		"GO_GALAXY_GIT_A_URL":          "https://a.example",
		"GO_GALAXY_GIT_A_USERNAME":     "ua",
		"GO_GALAXY_GIT_A_PASSWORD":     gitPasswordPlaintext,
		"GO_GALAXY_GIT_A_TOKEN":        "x",
		"GO_GALAXY_GIT_A_B_URL":        "https://b.example",
		"GO_GALAXY_GIT_A_B_USERNAME":   "ub",
		"GO_GALAXY_GIT_A_B_PASSWORD":   gitPasswordPlaintext,
		"GO_GALAXY_GIT_A_B_INSECURE":   "1",
		"GO_GALAXY_GIT_UNDECLARED_URL": "https://c.example",
	})
	cfg := &Config{}

	if err := loadGitCredentials(cfg); err != nil {
		t.Fatalf("loadGitCredentials() error = %v, want nil", err)
	}
	want := []string{
		"unsupported variable GO_GALAXY_GIT_A_B_INSECURE ignored",
		"unsupported variable GO_GALAXY_GIT_A_TOKEN ignored",
	}
	if fmt.Sprint(cfg.Warnings) != fmt.Sprint(want) {
		t.Fatalf("Warnings = %q, want %q", cfg.Warnings, want)
	}
}

func gitRedactionFixture() GitCredential {
	return GitCredential{
		ID:            "hub",
		URL:           mustPrefix("https://git.example/org"),
		Username:      "ci",
		Password:      NewSecret(gitPasswordPlaintext),
		SSHKeyPEM:     NewSecret(gitKeyPlaintext),
		SSHPassphrase: NewSecret(gitPassphrasePlaintext),
		Kind:          GitCredentialBasic,
	}
}

// TestGitCredentialRedactsSecrets is TestS3CacheConfigRedactsSecrets for the
// git shape; its fixture carries all three secrets at once, which no loaded
// credential does, so a rendering leaking any one of them fails.
func TestGitCredentialRedactsSecrets(t *testing.T) {
	t.Parallel()
	cred := gitRedactionFixture()

	// Encoding is the subject here, as in the S3 test: musttag wants tags on
	// a struct production never encodes.
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
		mustNotContainSecrets(t, verb+": "+out)
		if !strings.Contains(out, "git.example") {
			t.Errorf("%s output lost the binding URL: %s", verb, out)
		}
	}
}

// TestGitCredentialSecretsAreStillReadable is the positive control.
func TestGitCredentialSecretsAreStillReadable(t *testing.T) {
	t.Parallel()
	cred := gitRedactionFixture()
	if cred.Password.Reveal() != gitPasswordPlaintext || cred.SSHKeyPEM.Reveal() != gitKeyPlaintext ||
		cred.SSHPassphrase.Reveal() != gitPassphrasePlaintext {
		t.Fatalf("Reveal() does not return the plaintexts the fixture was built with")
	}
}

// mustPrefix parses a binding URL the way loadGitCredentials does, for the
// expected values; the raw strings are fixed literals, so a parse failure is
// a fixture bug rather than a case.
func mustPrefix(raw string) gitsource.URL {
	u, err := gitsource.ParsePrefix(raw)
	if err != nil {
		panic(err)
	}
	return u
}

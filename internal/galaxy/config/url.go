package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// URLCredential is one origin-bound url-source Bearer token: the declared id,
// the binding prefix (urlsource.ParsePrefix) and the Secret token, whose
// plaintext only the command wiring that builds a fetch.URLBinding reads.
type URLCredential struct {
	ID    string
	Token Secret
	URL   urlsource.Prefix
}

const (
	// urlCredentialsListEnv lists the declared credential ids; nothing else
	// under the prefix is read for an id that is not listed here.
	urlCredentialsListEnv = "GO_GALAXY_URL_CREDENTIALS" //nolint:gosec // an environment variable name, not a credential
	// urlCredentialEnvPrefix starts every per-id GO_GALAXY_URL_<ID>_<KEY>; <ID>
	// is upper-cased as a git id is, so two ids differing only in case are refused.
	urlCredentialEnvPrefix = "GO_GALAXY_URL_" //nolint:gosec // an environment variable name prefix, not a credential

	urlKeyURL   = "URL"
	urlKeyToken = "TOKEN"
)

// urlCredentialKeys is every key this tool reads for a declared id; the
// unknown-variable scan treats any other name under the id's prefix as
// unsupported.
func urlCredentialKeys() []string {
	return []string{urlKeyURL, urlKeyToken}
}

// loadURLCredentials fills cfg.URLCredentials from GO_GALAXY_URL_CREDENTIALS and
// each id's GO_GALAXY_URL_<ID>_{URL,TOKEN}; empty counts as unset. Checks run in
// a fixed order, and a refusal names the variable or id, never a raw value.
func loadURLCredentials(cfg *Config) error {
	ids, err := credentialIDList(os.Getenv(urlCredentialsListEnv),
		urlCredentialsListEnv, urlCredentialEnvPrefix, helpers.ErrURLCredentialInvalid)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	creds := make([]URLCredential, 0, len(ids))
	for _, id := range ids {
		cred, err := buildURLCredential(id)
		if err != nil {
			return err
		}
		creds = append(creds, cred)
	}
	if err := checkURLCredentialURLConflicts(creds); err != nil {
		return err
	}
	cfg.Warnings = append(cfg.Warnings,
		unknownCredentialVariableWarnings(ids, os.Environ(), urlCredentialKeys(), urlCredentialVar)...)
	cfg.URLCredentials = creds
	return nil
}

// urlCredentialVar composes the variable name for one key of one id.
func urlCredentialVar(id, key string) string {
	return urlCredentialEnvPrefix + strings.ToUpper(id) + "_" + key
}

// buildURLCredential settles one id, URL before token. The URL is trimmed (a CI
// secret's trailing newline), the token taken verbatim: it may end in whitespace.
func buildURLCredential(id string) (URLCredential, error) {
	urlVar := urlCredentialVar(id, urlKeyURL)
	rawURL := strings.TrimSpace(os.Getenv(urlVar))
	if rawURL == "" {
		return URLCredential{}, fmt.Errorf("%w: %s is not set", helpers.ErrURLCredentialInvalid, urlVar)
	}
	parsed, err := urlsource.ParsePrefix(rawURL)
	if err != nil {
		// ParsePrefix's own error already wraps the sentinel; this adds the
		// variable the value came from without echoing the value.
		return URLCredential{}, fmt.Errorf("%s: %w", urlVar, err)
	}
	token := os.Getenv(urlCredentialVar(id, urlKeyToken))
	if token == "" {
		return URLCredential{}, fmt.Errorf("%w: %s is not set", helpers.ErrURLCredentialInvalid, urlCredentialVar(id, urlKeyToken))
	}
	if parsed.Scheme == "http" && !parsed.IsLoopback() {
		return URLCredential{}, fmt.Errorf("%w: url credential %q (%s)", helpers.ErrInsecureTokenTransport, id, parsed.Origin())
	}
	return URLCredential{ID: id, URL: parsed, Token: NewSecret(token)}, nil
}

// checkURLCredentialURLConflicts refuses two ids bound to one canonical prefix:
// the transport's longest-prefix match relies on it to make a tie impossible.
func checkURLCredentialURLConflicts(creds []URLCredential) error {
	seen := make(map[string]string, len(creds))
	for _, cred := range creds {
		key := cred.URL.String()
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("%w: %s and %s bind the same URL", helpers.ErrURLCredentialInvalid,
				urlCredentialVar(prior, urlKeyURL), urlCredentialVar(cred.ID, urlKeyURL))
		}
		seen[key] = cred.ID
	}
	return nil
}

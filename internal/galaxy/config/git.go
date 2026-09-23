package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// GitCredentialKind tells a Basic (http(s)) binding from an ssh key binding.
// It is checked against the binding URL's scheme at load time, so a consumer
// never has to decide which of the two field groups applies.
type GitCredentialKind uint8

const (
	// GitCredentialBasic is a username and password sent as HTTP Basic auth
	// to an https (or loopback http) origin.
	GitCredentialBasic GitCredentialKind = iota + 1
	// GitCredentialSSHKey is a PEM private key, with an optional passphrase,
	// offered to an ssh origin.
	GitCredentialSSHKey
)

// GitCredential is one host-bound git credential: its id, the binding URL
// and the Secret fields of exactly one Kind. The plaintext leaves this form
// only in the command wiring that builds a gitsource.Credential from it.
type GitCredential struct {
	ID            string
	Username      string
	Password      Secret
	SSHKeyPEM     Secret
	SSHPassphrase Secret
	URL           gitsource.URL
	Kind          GitCredentialKind
}

const (
	// gitCredentialsListEnv lists the declared credential ids; nothing else
	// under the prefix is read for an id that is not listed here.
	gitCredentialsListEnv = "GO_GALAXY_GIT_CREDENTIALS" //nolint:gosec // an environment variable name, not a credential
	// gitCredentialEnvPrefix prefixes every GO_GALAXY_GIT_<ID>_<KEY> variable;
	// <ID> is upper-cased, so ids differing only in case are refused.
	gitCredentialEnvPrefix = "GO_GALAXY_GIT_" //nolint:gosec // an environment variable name prefix, not a credential

	gitKeyURL              = "URL"
	gitKeyUsername         = "USERNAME"
	gitKeyPassword         = "PASSWORD"
	gitKeySSHKey           = "SSH_KEY"
	gitKeySSHKeyFile       = "SSH_KEY_FILE"
	gitKeySSHKeyPassphrase = "SSH_KEY_PASSPHRASE" //nolint:gosec // an environment variable name suffix, not a credential

	// gitSchemeSSH is the one binding scheme an ssh key applies to; the
	// other two schemes gitsource.ParsePrefix admits take Basic.
	gitSchemeSSH = "ssh"
)

// gitCredentialKeys is every key this tool reads for a declared id; the
// unknown-variable scan treats any other name under the id's prefix as
// unsupported.
func gitCredentialKeys() []string {
	return []string{gitKeyURL, gitKeyUsername, gitKeyPassword, gitKeySSHKey, gitKeySSHKeyFile, gitKeySSHKeyPassphrase}
}

// gitCredentialEnv is the raw per-id variable surface, read once per id so
// every rule below judges the same snapshot and names the variable it read.
type gitCredentialEnv struct {
	id         string
	url        string
	username   string
	password   string
	sshKey     string
	sshKeyFile string
	passphrase string
}

// loadGitCredentials fills cfg.GitCredentials from GO_GALAXY_GIT_CREDENTIALS
// and each id's GO_GALAXY_GIT_<ID>_* variables; an empty value counts as unset.
// Refusals name the offending variable and never echo a secret value.
func loadGitCredentials(cfg *Config) error {
	ids, err := gitCredentialIDs(os.Getenv(gitCredentialsListEnv))
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	creds := make([]GitCredential, 0, len(ids))
	for _, id := range ids {
		cred, err := buildGitCredential(readGitCredentialEnv(id))
		if err != nil {
			return err
		}
		creds = append(creds, cred)
	}
	if err := checkGitCredentialURLConflicts(creds); err != nil {
		return err
	}
	cfg.Warnings = append(cfg.Warnings, unknownGitVariableWarnings(ids, os.Environ())...)
	cfg.GitCredentials = creds
	return nil
}

// gitCredentialIDs splits the id list under the shared rules
// credentialIDList states, refusing with this surface's sentinel.
func gitCredentialIDs(raw string) ([]string, error) {
	return credentialIDList(raw, gitCredentialsListEnv, gitCredentialEnvPrefix, helpers.ErrGitCredentialInvalid)
}

// gitCredentialVar composes the variable name for one key of one id.
func gitCredentialVar(id, key string) string {
	return gitCredentialEnvPrefix + strings.ToUpper(id) + "_" + key
}

// readGitCredentialEnv snapshots the six variables of one id, trimming the
// non-secret values against a CI store's trailing newline and taking secrets
// verbatim, since a password or a PEM may legitimately end in whitespace.
func readGitCredentialEnv(id string) gitCredentialEnv {
	return gitCredentialEnv{
		id:         id,
		url:        strings.TrimSpace(os.Getenv(gitCredentialVar(id, gitKeyURL))),
		username:   strings.TrimSpace(os.Getenv(gitCredentialVar(id, gitKeyUsername))),
		password:   os.Getenv(gitCredentialVar(id, gitKeyPassword)),
		sshKey:     os.Getenv(gitCredentialVar(id, gitKeySSHKey)),
		sshKeyFile: strings.TrimSpace(os.Getenv(gitCredentialVar(id, gitKeySSHKeyFile))),
		passphrase: os.Getenv(gitCredentialVar(id, gitKeySSHKeyPassphrase)),
	}
}

// buildGitCredential settles one id: its URL first, then which kind the
// scheme demands and whether the variables present form exactly that kind.
func buildGitCredential(env gitCredentialEnv) (GitCredential, error) {
	urlVar := gitCredentialVar(env.id, gitKeyURL)
	if env.url == "" {
		return GitCredential{}, fmt.Errorf("%w: %s is not set", helpers.ErrGitCredentialInvalid, urlVar)
	}
	parsed, err := gitsource.ParsePrefix(env.url)
	if err != nil {
		return GitCredential{}, fmt.Errorf("%w: %s: %w", helpers.ErrGitCredentialInvalid, urlVar, err)
	}

	basicVars := env.presentVars(gitKeyUsername, gitKeyPassword)
	sshVars := env.presentVars(gitKeySSHKey, gitKeySSHKeyFile, gitKeySSHKeyPassphrase)
	if parsed.Scheme == gitSchemeSSH {
		if len(basicVars) > 0 {
			return GitCredential{}, fmt.Errorf("%w: %s names an ssh URL, which takes an ssh key, not %s",
				helpers.ErrGitCredentialInvalid, urlVar, strings.Join(basicVars, " and "))
		}
		return buildGitSSHCredential(env, parsed)
	}
	if len(sshVars) > 0 {
		return GitCredential{}, fmt.Errorf("%w: %s names an %s URL, which takes a username and password, not %s",
			helpers.ErrGitCredentialInvalid, urlVar, parsed.Scheme, strings.Join(sshVars, " and "))
	}
	return buildGitBasicCredential(env, parsed)
}

// presentVars returns the variable names, among keys, whose value is set.
func (env gitCredentialEnv) presentVars(keys ...string) []string {
	values := map[string]string{
		gitKeyUsername:         env.username,
		gitKeyPassword:         env.password,
		gitKeySSHKey:           env.sshKey,
		gitKeySSHKeyFile:       env.sshKeyFile,
		gitKeySSHKeyPassphrase: env.passphrase,
	}
	var present []string
	for _, key := range keys {
		if values[key] != "" {
			present = append(present, gitCredentialVar(env.id, key))
		}
	}
	return present
}

// buildGitBasicCredential requires both halves of a Basic pair, since either
// alone is a misconfiguration, and refuses plaintext http off loopback with
// helpers.ErrInsecureTokenTransport, as a Galaxy token is refused there.
func buildGitBasicCredential(env gitCredentialEnv, parsed gitsource.URL) (GitCredential, error) {
	userVar := gitCredentialVar(env.id, gitKeyUsername)
	passVar := gitCredentialVar(env.id, gitKeyPassword)
	switch {
	case env.username == "" && env.password == "":
		return GitCredential{}, fmt.Errorf("%w: id %q configures neither %s and %s nor an ssh key",
			helpers.ErrGitCredentialInvalid, env.id, userVar, passVar)
	case env.username == "":
		return GitCredential{}, fmt.Errorf("%w: %s is set without %s", helpers.ErrGitCredentialInvalid, passVar, userVar)
	case env.password == "":
		return GitCredential{}, fmt.Errorf("%w: %s is set without %s", helpers.ErrGitCredentialInvalid, userVar, passVar)
	}
	if parsed.Scheme == "http" && !parsed.IsLoopback() {
		return GitCredential{}, fmt.Errorf("%w: git credential %q (%s)", helpers.ErrInsecureTokenTransport, env.id, parsed.Origin())
	}
	return GitCredential{
		ID:       env.id,
		URL:      parsed,
		Username: env.username,
		Password: NewSecret(env.password),
		Kind:     GitCredentialBasic,
	}, nil
}

// buildGitSSHCredential requires exactly one source for the key and reads a
// file-sourced key now, so the PEM is held the same way whichever variable
// supplied it and a bad path fails before the run reaches a remote.
func buildGitSSHCredential(env gitCredentialEnv, parsed gitsource.URL) (GitCredential, error) {
	keyVar := gitCredentialVar(env.id, gitKeySSHKey)
	fileVar := gitCredentialVar(env.id, gitKeySSHKeyFile)
	switch {
	case env.sshKey != "" && env.sshKeyFile != "":
		return GitCredential{}, fmt.Errorf("%w: %s and %s are both set; the key comes from one of them",
			helpers.ErrGitCredentialInvalid, keyVar, fileVar)
	case env.sshKey == "" && env.sshKeyFile == "" && env.passphrase != "":
		return GitCredential{}, fmt.Errorf("%w: %s is set without %s or %s",
			helpers.ErrGitCredentialInvalid, gitCredentialVar(env.id, gitKeySSHKeyPassphrase), keyVar, fileVar)
	case env.sshKey == "" && env.sshKeyFile == "":
		return GitCredential{}, fmt.Errorf("%w: id %q configures neither %s nor %s nor a username and password",
			helpers.ErrGitCredentialInvalid, env.id, keyVar, fileVar)
	}
	pem := env.sshKey
	if env.sshKeyFile != "" {
		// The path is operator-configured from the environment, never
		// repository content, which is why reading it is not an inclusion
		// this tool has to defend against.
		data, err := os.ReadFile(filepath.Clean(env.sshKeyFile))
		if err != nil {
			return GitCredential{}, fmt.Errorf("%w: %s names a key this process cannot read: %w",
				helpers.ErrGitCredentialInvalid, fileVar, err)
		}
		pem = string(data)
	}
	return GitCredential{
		ID:            env.id,
		URL:           parsed,
		SSHKeyPEM:     NewSecret(pem),
		SSHPassphrase: NewSecret(env.passphrase),
		Kind:          GitCredentialSSHKey,
	}, nil
}

// checkGitCredentialURLConflicts refuses two ids bound to one canonical URL:
// gitsource.MatchCredential's longest-prefix match relies on it to make a tie
// impossible, which would otherwise be settled silently by list order.
func checkGitCredentialURLConflicts(creds []GitCredential) error {
	seen := make(map[string]string, len(creds))
	for _, cred := range creds {
		key := cred.URL.String()
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("%w: %s and %s bind the same URL", helpers.ErrGitCredentialInvalid,
				gitCredentialVar(prior, gitKeyURL), gitCredentialVar(cred.ID, gitKeyURL))
		}
		seen[key] = cred.ID
	}
	return nil
}

// unknownGitVariableWarnings returns one warning per environment variable
// that sits under a declared id's prefix and is none of the six keys, under
// the shared scan unknownCredentialVariableWarnings states.
func unknownGitVariableWarnings(ids []string, environ []string) []string {
	return unknownCredentialVariableWarnings(ids, environ, gitCredentialKeys(), gitCredentialVar)
}

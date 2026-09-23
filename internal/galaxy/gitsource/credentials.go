package gitsource

import "strings"

// Credential is one host-bound git credential in clear form: the binding URL
// and Basic or ssh-key secrets. Only the command wiring builds it, from
// config.GitCredential's Secret values, and it is never persisted or printed.
type Credential struct {
	URL           URL
	Username      string
	Password      string
	SSHPassphrase string
	SSHKey        []byte
}

// IsZero reports whether the credential binds nothing: the value
// MatchCredential returns when no binding covers a URL.
func (c Credential) IsZero() bool {
	return c.URL.Host == "" && c.Username == "" && c.Password == "" && len(c.SSHKey) == 0
}

// MatchCredential returns the credential bound to u: same origin and u's path
// at or beneath the binding path, longest prefix winning. The ssh user is not
// matched; duplicate bindings are refused at configuration, so no tie occurs.
func MatchCredential(u URL, creds []Credential) (Credential, bool) {
	var best Credential
	bestLen := -1
	for _, cred := range creds {
		if cred.URL.Origin() != u.Origin() {
			continue
		}
		// Both sides are compared without their leading slash so a binding
		// written as ssh://host/org also covers the scp-like git@host:org/repo,
		// whose path is spelled relative.
		prefix := strings.TrimPrefix(cred.URL.Path, "/")
		path := strings.TrimPrefix(u.Path, "/")
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		if len(prefix) > bestLen {
			best, bestLen = cred, len(prefix)
		}
	}
	return best, bestLen >= 0
}

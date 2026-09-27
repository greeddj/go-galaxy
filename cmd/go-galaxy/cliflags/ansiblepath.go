package cliflags

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// ansiblePathEnvSource is an ANSIBLE_* path variable as a flag source, its ~
// and $VAR expanded as ansible expands them. urfave consults it only when no
// earlier source set the flag, so a flag or GO_GALAXY_* value stays literal.
type ansiblePathEnvSource struct {
	key  string
	list bool
}

// ansibleSources is cli.EnvVars over goGalaxyKeys, then ansibleKey read as an
// ansiblePathEnvSource, a ":" list when list is set: source order stays
// precedence, the ANSIBLE_ spelling last, a relative result cwd-relative as in ansible.
func ansibleSources(goGalaxyKeys []string, ansibleKey string, list bool) cli.ValueSourceChain {
	chain := cli.EnvVars(goGalaxyKeys...)
	chain.Chain = append(chain.Chain, &ansiblePathEnvSource{key: ansibleKey, list: list})
	return chain
}

// Lookup reports the variable's expanded value, and whether it is exported;
// an exported-empty variable is found and expands to "".
func (s *ansiblePathEnvSource) Lookup() (string, bool) {
	value, ok := os.LookupEnv(s.key)
	if !ok {
		return "", false
	}
	if !s.list {
		return expandAnsiblePathEntry(value), true
	}
	entries := strings.Split(value, ":")
	for i, entry := range entries {
		entries[i] = expandAnsiblePathEntry(entry)
	}
	return strings.Join(entries, ":"), true
}

// expandAnsiblePathEntry is one path expanded and normalized as ansible's
// unfrackpath does, left relative; an empty entry stays empty, so the refusal
// an empty install or cache path meets still fires.
func expandAnsiblePathEntry(entry string) string {
	if expanded := galaxyhelpers.ExpandAnsiblePath(entry); expanded != "" {
		return filepath.Clean(expanded)
	}
	return ""
}

// IsFromEnv marks the source as a variable, so help lists it with the rest.
func (s *ansiblePathEnvSource) IsFromEnv() bool {
	return true
}

// Key is the variable's name, as help prints it.
func (s *ansiblePathEnvSource) Key() string {
	return s.key
}

// String names the source in urfave's parse errors.
func (s *ansiblePathEnvSource) String() string {
	return fmt.Sprintf("environment variable %q", s.key)
}

// GoString renders the source for %#v.
func (s *ansiblePathEnvSource) GoString() string {
	return fmt.Sprintf("&ansiblePathEnvSource{Key:%q}", s.key)
}

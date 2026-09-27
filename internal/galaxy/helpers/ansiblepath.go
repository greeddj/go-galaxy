package helpers

import (
	"os"
	"os/user"
	"regexp"
	"strings"
)

// ansibleVarPattern is Python's posixpath._varpattern under re.ASCII: $name,
// ${name}, or a "${" with no closing brace, which expandVars leaves as written.
var ansibleVarPattern = regexp.MustCompile(`\$(\w+|\{[^}]*\}?)`)

// ExpandAnsiblePath expands path the way ansible's unfrackpath does before it
// joins anything: os.path.expandvars first, then os.path.expanduser, so a
// variable holding "~" is expanded too. It never makes a path absolute.
func ExpandAnsiblePath(path string) string {
	return expandUser(expandVars(path))
}

// expandVars is Python's os.path.expandvars: an exported variable's value
// replaces $name or ${name}, empty included; an unset name stays as written,
// and a substituted value is not scanned again.
func expandVars(path string) string {
	if !strings.Contains(path, "$") {
		return path
	}
	return ansibleVarPattern.ReplaceAllStringFunc(path, func(match string) string {
		name := match[1:]
		if strings.HasPrefix(name, "{") {
			if !strings.HasSuffix(name, "}") {
				return match
			}
			name = name[1 : len(name)-1]
		}
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return match
	})
}

// expandUser is Python's os.path.expanduser: "~" up to the first "/" becomes
// $HOME, or this user's password-database home when HOME is unset, and "~name"
// that user's home; a home that cannot be found leaves path as written.
func expandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	end := len(path)
	if i := strings.IndexByte(path[1:], '/'); i >= 0 {
		end = i + 1
	}
	home, ok := userHome(path[1:end])
	if !ok {
		return path
	}
	if expanded := strings.TrimRight(home, "/") + path[end:]; expanded != "" {
		return expanded
	}
	return "/"
}

// userHome returns the home directory "~name" stands for, "" naming the
// current user, and whether one was found.
func userHome(name string) (string, bool) {
	if name == "" {
		if home, ok := os.LookupEnv("HOME"); ok {
			return home, true
		}
		current, err := user.Current()
		if err != nil {
			return "", false
		}
		return current.HomeDir, true
	}
	named, err := user.Lookup(name)
	if err != nil {
		return "", false
	}
	return named.HomeDir, true
}

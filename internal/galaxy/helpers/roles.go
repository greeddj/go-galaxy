package helpers

import (
	"fmt"
	"strings"
)

// Role identifiers have their own predicates, not the collection alphabet:
// real role names carry hyphens, upper-case letters and leading digits
// (geerlingguy.php-versions, 6connect.x).

const (
	// roleNamePartMaxLen bounds one half of a Galaxy role name. The owner is a
	// GitHub login (at most 39 characters) and the role name is short, so 64
	// leaves room without admitting a value no server could have minted.
	roleNamePartMaxLen = 64
	// roleInstallNameMaxLen bounds the directory a role installs into. A
	// Galaxy name is at most 2*roleNamePartMaxLen+1; a git role's default
	// name is a repository name, which GitHub caps at 100.
	roleInstallNameMaxLen = 128
	// roleVersionMaxLen bounds a role version, which is a git ref name or a
	// tag; git itself imposes no cap, this tool does so a version fits in a
	// message and a filename.
	roleVersionMaxLen = 128
	// roleNameParts is the two halves of a Galaxy role name, owner.role.
	roleNameParts = 2
)

// runesMatch reports whether s is non-empty, its first rune satisfies
// first and every later rune satisfies rest.
func runesMatch(s string, first, rest func(r rune) bool) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !first(r) || i > 0 && !rest(r) {
			return false
		}
	}
	return true
}

func isASCIIAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// IsRoleNamePart reports whether part is a Galaxy role owner or role under
// ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$, the GitHub login alphabet plus the
// underscore legacy namespaces carry; no dot, so a name splits once.
func IsRoleNamePart(part string) bool {
	return len(part) <= roleNamePartMaxLen && runesMatch(part, isASCIIAlnum,
		func(r rune) bool { return isASCIIAlnum(r) || r == '_' || r == '-' })
}

// SplitRoleName splits a Galaxy role name at its one dot into owner and role,
// both satisfying IsRoleNamePart. ansible splits at the last dot; no GitHub
// login carries a dot, so a second one is refused rather than guessed at.
func SplitRoleName(value string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(value), ".")
	if len(parts) != roleNameParts || !IsRoleNamePart(parts[0]) || !IsRoleNamePart(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsRoleName reports whether value is a well-formed Galaxy role name,
// owner.role, as SplitRoleName accepts it.
func IsRoleName(value string) bool {
	_, _, ok := SplitRoleName(value)
	return ok
}

// IsRoleInstallName reports whether name may be a role's directory under
// roles_path: IsPathElement plus ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$, and
// never "ansible_collections", so a role cannot replace a collections tree.
func IsRoleInstallName(name string) bool {
	if !IsPathElement(name) || len(name) > roleInstallNameMaxLen || name == "ansible_collections" {
		return false
	}
	return runesMatch(name, func(r rune) bool { return isASCIIAlnum(r) || r == '_' },
		func(r rune) bool { return isASCIIAlnum(r) || r == '_' || r == '.' || r == '-' })
}

// IsRoleVersion reports whether s is a role version read back from a store
// record or lockfile, under a strict subset of git's ref-name rules. It is not
// a path element rule: "/" is legal, and ArtifactKey escapes it in a filename.
func IsRoleVersion(s string) bool {
	if len(s) > roleVersionMaxLen || strings.Contains(s, "..") || strings.Contains(s, "//") ||
		strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".lock") {
		return false
	}
	return runesMatch(s, isASCIIAlnum,
		func(r rune) bool { return isASCIIAlnum(r) || strings.ContainsRune("._/+-", r) })
}

// RoleArtifactFilename composes a role artifact's cache filename, shared by
// the role install and cleanup. The "role." prefix keeps it apart from a
// collection built from the same repository and commit.
func RoleArtifactFilename(name, version string) string {
	return fmt.Sprintf("role.%s-%s.tar.gz", name, version)
}

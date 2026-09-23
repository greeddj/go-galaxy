package gitsource

import (
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// RefKind classifies what a requirement's version names for a git source.
type RefKind uint8

const (
	// RefHEAD is the remote's HEAD, what an absent, empty or "*" version
	// means.
	RefHEAD RefKind = iota
	// RefName is an unqualified name: a branch if the remote advertises
	// refs/heads/<name>, else a tag if it advertises refs/tags/<name>.
	RefName
	// RefCommit is a full forty-hex commit hash.
	RefCommit
	// RefQualified is a fully spelled refs/heads/... or refs/tags/... name.
	RefQualified
)

const (
	refsHeadsPrefix = "refs/heads/"
	refsTagsPrefix  = "refs/tags/"
	commitHexLen    = 40
	// abbreviatedMinLen is the shortest all-hex name read as a shortened
	// commit rather than a branch or tag: git abbreviates to seven digits,
	// and a shorter hex run is more likely a chosen name.
	abbreviatedMinLen = 7
)

// Ref is a classified ref. Name is canonical: "HEAD", the unqualified name,
// the lower-cased forty-hex commit, or the qualified refs/... spelling.
type Ref struct {
	Name string
	Kind RefKind
}

// IsCommit reports whether the ref pins a commit by hash.
func (r Ref) IsCommit() bool { return r.Kind == RefCommit }

// String renders the canonical name.
func (r Ref) String() string { return r.Name }

// ParseRef classifies s: empty, "*" or "HEAD" is HEAD; forty hex digits a
// commit; seven to thirty-nine hex digits are refused as abbreviated; refs/heads/
// or refs/tags/ is qualified; else unqualified, all under git's check-ref-format.
func ParseRef(s string) (Ref, error) {
	name := strings.TrimSpace(s)
	if name == "" || name == "*" || name == headRef {
		return Ref{Name: headRef, Kind: RefHEAD}, nil
	}
	if ref, ok, err := parseHexRef(name); ok || err != nil {
		return ref, err
	}
	return parseNamedRef(name)
}

// parseHexRef classifies an all-hex name: ok is false when the name is not hex
// or is too short to be read as a commit and therefore stays a branch or tag
// name for parseNamedRef.
func parseHexRef(name string) (Ref, bool, error) {
	if !isHex(name) {
		return Ref{}, false, nil
	}
	switch {
	case len(name) == commitHexLen:
		return Ref{Name: strings.ToLower(name), Kind: RefCommit}, true, nil
	case len(name) >= abbreviatedMinLen && len(name) < commitHexLen:
		return Ref{}, true, fmt.Errorf("%w: %q; spell the full %d-digit commit, or a branch as %s%s",
			helpers.ErrGitAbbreviatedCommit, name, commitHexLen, refsHeadsPrefix, name)
	default:
		return Ref{}, false, nil
	}
}

// parseNamedRef classifies a non-hex name as qualified or unqualified, after
// git's check-ref-format rules.
func parseNamedRef(name string) (Ref, error) {
	if strings.HasPrefix(name, refsHeadsPrefix) || strings.HasPrefix(name, refsTagsPrefix) {
		if err := checkRefFormat(name); err != nil {
			return Ref{}, err
		}
		return Ref{Name: name, Kind: RefQualified}, nil
	}
	if strings.HasPrefix(name, "refs/") {
		return Ref{}, fmt.Errorf("%w: %q: only %s and %s are accepted as qualified refs",
			helpers.ErrInvalidGitRef, name, refsHeadsPrefix, refsTagsPrefix)
	}
	if err := checkRefFormat(refsHeadsPrefix + name); err != nil {
		return Ref{}, err
	}
	return Ref{Name: name, Kind: RefName}, nil
}

// IsCommitHash reports whether s is exactly forty lowercase hex digits, the
// canonical commit spelling a locator and a lockfile carry.
func IsCommitHash(s string) bool {
	if len(s) != commitHexLen {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// checkRefFormat applies git's check-ref-format rules to a qualified name,
// including git's refusal of a component starting with "-" (it reads as an
// option), so nothing asked of a remote is a ref git would refuse to create.
func checkRefFormat(name string) error {
	reject := func(reason string) error {
		return fmt.Errorf("%w: %q: %s", helpers.ErrInvalidGitRef, name, reason)
	}
	if reason := refShapeProblem(name); reason != "" {
		return reject(reason)
	}
	for _, r := range name {
		if isForbiddenRefRune(r) {
			return reject(fmt.Sprintf("a ref name cannot contain %q", r))
		}
	}
	for component := range strings.SplitSeq(name, "/") {
		if reason := refComponentProblem(component); reason != "" {
			return reject(reason)
		}
	}
	return nil
}

// refShapeProblem returns the reason the whole name is refused on its shape
// alone, before the per-rune and per-component rules, or "" when it passes.
func refShapeProblem(name string) string {
	switch {
	case name == "@" || strings.Contains(name, "..") || strings.Contains(name, "@{"):
		return "a ref name cannot be \"@\" or contain \"..\" or \"@{\""
	case strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//"):
		return "a ref name cannot start or end with \"/\" or contain \"//\""
	case strings.HasSuffix(name, "."):
		return "a ref name cannot end with \".\""
	default:
		return ""
	}
}

// isForbiddenRefRune reports whether r may never appear in a ref name: a
// control rune, a space, or one of "~^:?*[\".
func isForbiddenRefRune(r rune) bool {
	return r < 0x20 || r == 0x7f || r == ' ' || strings.ContainsRune("~^:?*[\\", r)
}

// refComponentProblem returns the reason a single slash-separated component
// is refused, or "" when it is acceptable.
func refComponentProblem(component string) string {
	if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
		return "a ref component cannot start with \".\" or end with \".lock\""
	}
	if strings.HasPrefix(component, "-") || component == "@" {
		return "a ref component cannot start with \"-\" or be \"@\""
	}
	return ""
}

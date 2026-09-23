package helpers

import "github.com/Masterminds/semver/v3"

// IsExactVersion reports whether s parses as one semantic version, not a
// constraint such as "*". Acceptance implies IsPathElement under either
// semver.CoerceNewVersion setting, so no second path check is needed.
func IsExactVersion(s string) bool {
	_, err := semver.NewVersion(s)
	return err == nil
}

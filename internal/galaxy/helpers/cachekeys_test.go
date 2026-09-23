package helpers

import "testing"

// TestArtifactKeyIsFlat pins that ArtifactKey never produces a "/", which both
// backends' flat layouts rely on, even for filenames needing percent-encoding.
func TestArtifactKeyIsFlat(t *testing.T) {
	t.Parallel()
	cases := []string{
		"ns-name-1.0.0.tar.gz",
		"ns/name-1.0.0.tar.gz",
		"weird name/with spaces-1.0.0.tar.gz",
	}
	for _, filename := range cases {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			key := ArtifactKey("https://galaxy.example.com", filename)
			// A plain inner loop: it scans the runes of one already-named
			// case rather than iterating cases of its own.
			for _, r := range key {
				if r == '/' {
					t.Fatalf("ArtifactKey(%q) = %q, contains a %q", filename, key, "/")
				}
			}
		})
	}
}

// TestArtifactKeyDifferentBasesYieldDifferentPrefixes pins that two server
// bases publishing the identical filename get distinct keys.
func TestArtifactKeyDifferentBasesYieldDifferentPrefixes(t *testing.T) {
	t.Parallel()
	const filename = "ns-name-1.0.0.tar.gz"
	keyA := ArtifactKey("https://a.example.com", filename)
	keyB := ArtifactKey("https://b.example.com", filename)
	if keyA == keyB {
		t.Fatalf("ArtifactKey produced the same key %q for two different server bases", keyA)
	}
}

// TestArtifactKeyStableForSameBase pins that ArtifactKey is deterministic,
// since a later run's cache-hit lookup depends on getting the same key.
func TestArtifactKeyStableForSameBase(t *testing.T) {
	t.Parallel()
	const base = "https://galaxy.example.com"
	const filename = "ns-name-1.0.0.tar.gz"
	first := ArtifactKey(base, filename)
	second := ArtifactKey(base, filename)
	if first != second {
		t.Fatalf("ArtifactKey(%q, %q) = %q, then %q on a second call - want a stable result", base, filename, first, second)
	}
}

// TestArtifactKeyFingerprintLength pins the fingerprint prefix length so a
// future edit to ArtifactKeyFingerprintLen is a deliberate, visible change
// rather than an accidental one.
func TestArtifactKeyFingerprintLength(t *testing.T) {
	t.Parallel()
	key := ArtifactKey("https://galaxy.example.com", "ns-name-1.0.0.tar.gz")
	if len(key) <= ArtifactKeyFingerprintLen {
		t.Fatalf("ArtifactKey result %q is too short to contain a fingerprint prefix", key)
	}
	if key[ArtifactKeyFingerprintLen] != '.' {
		t.Fatalf("ArtifactKey result %q does not have '.' right after the %d-character fingerprint", key, ArtifactKeyFingerprintLen)
	}
}

// TestIsScopedArtifactKeyRecognizesArtifactKeyOutput proves IsScopedArtifactKey
// accepts every key ArtifactKey can actually produce, across bases and
// filenames that would themselves need percent-encoding.
func TestIsScopedArtifactKeyRecognizesArtifactKeyOutput(t *testing.T) {
	t.Parallel()
	// Rows are named rather than derived from base or filename: both carry
	// "/", which a derived subtest name would render as extra nesting.
	cases := []struct {
		name     string
		base     string
		filename string
	}{
		{"server root", "https://galaxy.example.com", "ns-name-1.0.0.tar.gz"},
		{"server with an api path", "https://a.example.com/api", "acme-app-1.0.0.tar.gz"},
		{"empty base, filename needing encoding", "", "weird name/with spaces-1.0.0.tar.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := ArtifactKey(tc.base, tc.filename)
			if !IsScopedArtifactKey(key) {
				t.Fatalf("IsScopedArtifactKey(%q) = false, want true for ArtifactKey(%q, %q)'s own output", key, tc.base, tc.filename)
			}
		})
	}
}

// TestIsScopedArtifactKeyRejectsUnscopedShapes pins that IsScopedArtifactKey
// refuses a legacy flat key and near misses of the fingerprint-then-"." shape.
func TestIsScopedArtifactKeyRejectsUnscopedShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"legacy flat key, no prefix at all", "ns-name-1.0.0.tar.gz"},
		{"too short to hold a fingerprint", "abc.def"},
		{"exactly fingerprint length, no separator at all", "0123456789ab"},
		{"fingerprint length with a non-dot separator", "0123456789ab-file.tar.gz"},
		{"uppercase hex fingerprint", "0123456789AB.file.tar.gz"},
		{"non-hex character in fingerprint", "0123456789ag.file.tar.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if IsScopedArtifactKey(tc.key) {
				t.Fatalf("IsScopedArtifactKey(%q) = true, want false", tc.key)
			}
		})
	}
}

// TestScopedDepsCacheKeyNeverCollidesWithOldFormat pins that no server base,
// empty included, makes ScopedDepsCacheKey yield the pre-scoping key shape:
// every scoped key carries DepsCacheKeySeparator.
func TestScopedDepsCacheKeyNeverCollidesWithOldFormat(t *testing.T) {
	t.Parallel()
	oldFormatKey := "acme.widgets@1.0.0"

	// Rows are named explicitly: an empty base renders as "#00", and a "/" in
	// a base splits the subtest path under -run.
	bases := []struct {
		name string
		base string
	}{
		{name: "server root", base: "https://galaxy.example.com"},
		{name: "server with an api path", base: "https://galaxy.example.com/api/v3"},
		{name: "empty base", base: ""},
	}
	for _, tc := range bases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ScopedDepsCacheKey(tc.base, oldFormatKey)
			if got == oldFormatKey {
				t.Fatalf("ScopedDepsCacheKey(%q, %q) = %q, collides with the old-format key", tc.base, oldFormatKey, got)
			}
			found := false
			for _, r := range got {
				if string(r) == DepsCacheKeySeparator {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("ScopedDepsCacheKey(%q, %q) = %q, does not contain the %q separator", tc.base, oldFormatKey, got, DepsCacheKeySeparator)
			}
		})
	}
}

// TestScopedDepsCacheKeyDistinguishesServers pins that two bases for the
// identical fqdn@version produce different deps-cache keys.
func TestScopedDepsCacheKeyDistinguishesServers(t *testing.T) {
	t.Parallel()
	const fqdnAtVersion = "acme.widgets@1.0.0"
	keyA := ScopedDepsCacheKey("https://a.example.com", fqdnAtVersion)
	keyB := ScopedDepsCacheKey("https://b.example.com", fqdnAtVersion)
	if keyA == keyB {
		t.Fatalf("ScopedDepsCacheKey produced the same key %q for two different server bases", keyA)
	}
}

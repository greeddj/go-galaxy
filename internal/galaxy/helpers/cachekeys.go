package helpers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
)

// ArtifactKeyFingerprintLen is how many leading hex characters of
// sha256(serverBase) prefix an artifact key: 48 bits makes a collision between
// two server bases astronomically unlikely while keeping keys legible.
const ArtifactKeyFingerprintLen = 12

// DepsCacheKeySeparator joins a scoped deps-cache key's server base to its
// "<ns>.<name>@<version>" suffix; "|" appears in no URL scheme and in no
// pre-scoping key, so the two key shapes can never collide.
const DepsCacheKeySeparator = "|"

// ScopedDepsCacheKey scopes fqdnAtVersion's deps-cache key to serverBase, which
// must be the base the collection actually resolved from, so two servers that
// publish one version with different dependencies never share an entry.
func ScopedDepsCacheKey(serverBase, fqdnAtVersion string) string {
	return serverBase + DepsCacheKeySeparator + fqdnAtVersion
}

// ArtifactFilename composes "<namespace>-<name>-<version>.tar.gz", which the
// writer (install, warm) and the deleter (cleanup) must agree on byte for byte:
// cleanup ignores Delete's error, so drift would silently stop purging tarballs.
func ArtifactFilename(namespace, name, version string) string {
	return fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
}

// ArtifactKey builds "<fp>.<QueryEscape(filename)>", fp a sha256 prefix of the
// resolved serverBase, so two servers never share a cache slot; the key holds
// no "/", which keeps both backends' layouts flat.
func ArtifactKey(serverBase, filename string) string {
	sum := sha256.Sum256([]byte(serverBase))
	fp := hex.EncodeToString(sum[:])[:ArtifactKeyFingerprintLen]
	return fp + "." + url.QueryEscape(filename)
}

// IsScopedArtifactKey reports whether key has ArtifactKey's shape and must
// change in lockstep with it; a key built from a walked name can match that
// shape, since url.QueryEscape leaves "." and "-" unescaped.
func IsScopedArtifactKey(key string) bool {
	if len(key) <= ArtifactKeyFingerprintLen || key[ArtifactKeyFingerprintLen] != '.' {
		return false
	}
	for i := range ArtifactKeyFingerprintLen {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

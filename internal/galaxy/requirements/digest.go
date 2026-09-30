package requirements

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// digestDomain opens every preimage Hash digests. A lockfile's canonical YAML
// never starts with it, so no requirements digest equals a lockfile hash; the
// version word changes only with the record layout below.
const digestDomain = "go-galaxy requirements digest v1\n"

// Hash returns the SHA256 hex of what f asks for: its collections, sorted,
// since their order changes no resolution, then its roles in file order,
// which decides the first-wins pick among their dependencies.
func (f File) Hash() string {
	collections := make([]string, 0, len(f.Collections))
	for _, c := range f.Collections {
		collections = append(collections, collectionRecord(c))
	}
	slices.Sort(collections)
	var b strings.Builder
	b.WriteString(digestDomain)
	for _, record := range collections {
		b.WriteString(record)
	}
	for _, r := range f.Roles {
		b.WriteString(digestRecord("role", r.Type, r.Name, r.Src, r.Version))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// collectionRecord renders one entry with a Galaxy constraint canonical and
// "*" for any version; a git or url entry's fields are already canonical,
// and signature sources are sorted with their queries cut, as the snapshot keeps them.
func collectionRecord(c CollectionRequirement) string {
	typ, version := c.Type, c.Version
	if !c.IsGit() && !c.IsURL() {
		typ = TypeGalaxy
		if version = helpers.CanonicalConstraint(version); version == "" {
			version = "*"
		}
	}
	signatures := make([]string, 0, len(c.Signatures))
	for _, source := range c.Signatures {
		if source = strings.TrimSpace(source); source != "" {
			signatures = append(signatures, helpers.WithoutQuery(source))
		}
	}
	slices.Sort(signatures)
	fields := append([]string{typ, c.Namespace, c.Name, version, c.Source, c.Ref, c.Subdir,
		strconv.Itoa(len(signatures))}, signatures...)
	return digestRecord("collection", fields...)
}

// digestRecord writes kind, then each field as its byte length, ":" and its
// bytes, then a newline: no field can run into the next, whatever it holds.
func digestRecord(kind string, fields ...string) string {
	var b strings.Builder
	b.WriteString(kind)
	for _, field := range fields {
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(len(field)))
		b.WriteByte(':')
		b.WriteString(field)
	}
	b.WriteByte('\n')
	return b.String()
}

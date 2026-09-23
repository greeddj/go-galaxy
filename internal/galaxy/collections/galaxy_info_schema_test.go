package collections

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
	"github.com/psvmcc/hub/pkg/types"
	"go.yaml.in/yaml/v3"
)

// ansibleSourceInfoKeys and ansibleSignatureKeys are ansible-core's closed
// GALAXY.yml schema, spelled out rather than derived from GalaxyYAML's tags.
//
//nolint:gochecknoglobals // fixed expectation tables, never mutated.
var (
	ansibleSourceInfoKeys = []string{
		"download_url", "format_version", "name", "namespace", "server", "signatures", "version", "version_url",
	}
	ansibleSignatureKeys = []string{"pubkey_fingerprint", "pulp_created", "signature", "signing_service"}
)

// schemaGitCollection and schemaURLCollection are a git and a url install of
// the same identity, each carrying the locator its source stamps on Source.
func schemaGitCollection() collection {
	return collection{
		Namespace: "acme", Name: "widgets", Version: testVersion100,
		Source: gitsource.Locator{URL: "https://git.example/acme/widgets.git", Commit: strings.Repeat("b", 40)}.String(),
	}
}

func schemaURLCollection() collection {
	return collection{
		Namespace: "acme", Name: "widgets", Version: testVersion100,
		Source: urlsource.Locator{URL: "https://files.example/acme-widgets-1.0.0.tar.gz", SHA256: strings.Repeat("a", 64)}.String(),
	}
}

// assertAnsibleSourceInfo fails unless data holds exactly the schema's keys,
// a non-null signatures list, and entries with only schema keys and the
// signature text ansible indexes them by.
func assertAnsibleSourceInfo(t *testing.T, data []byte) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("GALAXY.yml does not parse: %v\n%s", err, data)
	}
	keys := make([]string, 0, len(doc))
	for key := range doc {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, ansibleSourceInfoKeys) {
		t.Fatalf("GALAXY.yml keys = %v, want exactly ansible's %v:\n%s", keys, ansibleSourceInfoKeys, data)
	}
	list, ok := doc["signatures"].([]any)
	if !ok {
		t.Fatalf("signatures = %#v, want a list - ansible's verify --offline iterates it:\n%s", doc["signatures"], data)
	}
	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("signatures[%d] = %#v, want a mapping", i, item)
		}
		for key := range entry {
			if !slices.Contains(ansibleSignatureKeys, key) {
				t.Errorf("signatures[%d] carries %q, outside ansible's %v", i, key, ansibleSignatureKeys)
			}
		}
		if text, _ := entry["signature"].(string); text == "" {
			t.Errorf("signatures[%d] has no signature text, which ansible indexes every entry by", i)
		}
	}
}

// readInfoFile reads name from target's .info directory, reporting false
// when there is no such file.
func readInfoFile(t *testing.T, cfg *config.Config, target installTarget, name string) ([]byte, bool) {
	t.Helper()
	path := filepath.Join(cfg.DownloadPath, filepath.FromSlash(target.info), name)
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from this test's own t.TempDir
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data, true
}

// TestWriteGalaxyInfoConformsToAnsibleSchema pins that every source writes a
// GALAXY.yml in ansible's schema, even from odd server signatures, with git or
// url provenance in provenanceFileName and none for a Galaxy install.
func TestWriteGalaxyInfoConformsToAnsibleSchema(t *testing.T) {
	t.Parallel()

	signedMeta := newVersionInfo(acmeArtifactURL, "https://hub.example/api/v3/collections/acme/widgets/versions/1.0.0/")
	signedMeta.Signatures = []any{
		map[string]any{
			"signature": "-----BEGIN PGP SIGNATURE-----\nkept\n-----END PGP SIGNATURE-----\n", "pubkey_fingerprint": "ABCD",
			"signing_service": "ansible-default", "pulp_created": "2024-01-02T03:04:05.123456Z", "extra": "dropped",
		},
		"not an object",
		map[string]any{"pubkey_fingerprint": "no signature text"},
	}

	tests := []struct {
		meta       *types.GalaxyCollectionVersionInfo
		name       string
		provenance string
		col        collection
		signatures int
	}{
		{name: "galaxy with server signatures", col: collection{Namespace: "acme", Name: "widgets", Version: testVersion100},
			meta: signedMeta, signatures: 1},
		{name: "galaxy cache-hit fast path", col: collection{Namespace: "acme", Name: "widgets", Version: testVersion100}},
		{name: "git", col: schemaGitCollection(), provenance: "git_commit: " + strings.Repeat("b", 40) + "\n"},
		{name: "url", col: schemaURLCollection(), provenance: "url_sha256: " + strings.Repeat("a", 64) + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Server: "https://hub.example", DownloadPath: t.TempDir()}
			target := newTestInstallTarget(t, cfg, tt.col)

			if err := writeGalaxyInfo(target, cfg, tt.col, tt.meta); err != nil {
				t.Fatalf("writeGalaxyInfo: %v", err)
			}

			data, ok := readInfoFile(t, cfg, target, galaxyYAMLFileName)
			if !ok {
				t.Fatal("GALAXY.yml was not written")
			}
			assertAnsibleSourceInfo(t, data)
			var doc GalaxyYAML
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatalf("unmarshal GALAXY.yml: %v", err)
			}
			if len(doc.Signatures) != tt.signatures {
				t.Errorf("signatures = %+v, want %d entries", doc.Signatures, tt.signatures)
			}

			prov, ok := readInfoFile(t, cfg, target, provenanceFileName)
			switch {
			case tt.provenance == "" && ok:
				t.Errorf("%s written for a Galaxy install:\n%s", provenanceFileName, prov)
			case tt.provenance != "" && string(prov) != tt.provenance:
				t.Errorf("%s = %q, want %q", provenanceFileName, prov, tt.provenance)
			}
		})
	}
}

// TestGalaxySignaturesDecodeNeverFailsTheDocument pins that an odd signatures
// value costs the signatures, never the sidecar the skip check and outdated
// read, and re-renders in ansible's schema.
func TestGalaxySignaturesDecodeNeverFailsTheDocument(t *testing.T) {
	t.Parallel()

	const head = "namespace: acme\nname: widgets\nversion: 1.0.0\n"
	tests := []struct {
		name string
		yaml string
		want galaxySignatures
	}{
		{name: "null", yaml: "signatures: null\n"},
		{name: "absent", yaml: ""},
		{name: "a mapping instead of a list", yaml: "signatures: {signature: x}\n"},
		{name: "a scalar instead of a list", yaml: "signatures: text\n"},
		{
			name: "entries of every shape",
			yaml: "signatures:\n" +
				"- plain scalar\n" +
				"- [nested, list]\n" +
				"- {pubkey_fingerprint: no signature text}\n" +
				"- {signature: {nested: map}}\n" +
				"- {signature: kept, pulp_created: 2024-01-02T03:04:05Z, pubkey_fingerprint: 1234, extra: {deep: value}}\n",
			want: galaxySignatures{{Signature: "kept", PulpCreated: "2024-01-02T03:04:05Z", PubkeyFingerprint: "1234"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var doc GalaxyYAML
			if err := yaml.Unmarshal([]byte(head+tt.yaml), &doc); err != nil {
				t.Fatalf("the document failed to parse over its signatures: %v", err)
			}
			if !doc.describes(collection{Namespace: "acme", Name: "widgets", Version: testVersion100}) {
				t.Errorf("identity = %s.%s-%s, want acme.widgets-1.0.0", doc.Namespace, doc.Name, doc.Version)
			}
			if !slices.Equal(doc.Signatures, tt.want) {
				t.Errorf("signatures = %+v, want %+v", doc.Signatures, tt.want)
			}
			rendered, err := yaml.Marshal(&doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertAnsibleSourceInfo(t, rendered)
		})
	}
}

// legacySidecar renders the GALAXY.yml a release before provenanceFileName
// wrote for col: the provenance key inside the document, and signatures null.
func legacySidecar(col collection, provenanceKey string) []byte {
	return []byte("download_url: \"\"\nformat_version: 1.0.0\nname: " + col.Name + "\nnamespace: " + col.Namespace +
		"\nserver: https://origin.example\nsignatures: null\nversion: " + col.Version + "\nversion_url: \"\"\n" +
		provenanceKey + "\n")
}

// TestReconcileGalaxyInfoRepairsASidecarAnEarlierReleaseWrote pins that a skip
// repairs a legacy sidecar into the schema, moving provenance to its own file,
// and that a second skip writes nothing.
func TestReconcileGalaxyInfoRepairsASidecarAnEarlierReleaseWrote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		legacy   string
		col      collection
		wantKind installedKind
	}{
		{name: "git", col: schemaGitCollection(), legacy: "git_commit: " + strings.Repeat("b", 40), wantKind: installedGit},
		{name: "url", col: schemaURLCollection(), legacy: "url_sha256: " + strings.Repeat("a", 64), wantKind: installedURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, st := seedInstalledCollection(t, tt.col, legacySidecar(tt.col, tt.legacy))
			cfg := &config.Config{DownloadPath: filepath.Dir(filepath.Dir(filepath.Dir(target.path)))}
			runtime := infra.New(noopPrinter{}, nil)

			state, ok := canSkipInstall(target, tt.col, st, noopPrinter{})
			if !ok {
				t.Fatal("canSkipInstall = false; a sidecar outside the schema must be repaired, not reinstalled over")
			}
			reconcileGalaxyInfo(runtime, target, cfg, tt.col, state)

			data, _ := readInfoFile(t, cfg, target, galaxyYAMLFileName)
			assertAnsibleSourceInfo(t, data)
			if prov, _ := readInfoFile(t, cfg, target, provenanceFileName); string(prov) != tt.legacy+"\n" {
				t.Errorf("%s = %q, want the provenance moved out of GALAXY.yml: %q", provenanceFileName, prov, tt.legacy+"\n")
			}
			_, prov, ok := readInstalledSidecar(target.root, cfg, runtime, path.Base(target.info))
			if !ok || installedKindOf(prov) != tt.wantKind {
				t.Errorf("outdated reads kind %v (ok=%v) after the repair, want %v", installedKindOf(prov), ok, tt.wantKind)
			}

			galaxyPath := filepath.Join(cfg.DownloadPath, filepath.FromSlash(target.info), galaxyYAMLFileName)
			provPath := filepath.Join(cfg.DownloadPath, filepath.FromSlash(target.info), provenanceFileName)
			galaxyBefore, provBefore := mustModTime(t, galaxyPath), mustModTime(t, provPath)
			again, ok := canSkipInstall(target, tt.col, st, noopPrinter{})
			if !ok {
				t.Fatal("canSkipInstall = false over the repaired sidecar")
			}
			reconcileGalaxyInfo(runtime, target, cfg, tt.col, again)
			if !mustModTime(t, galaxyPath).Equal(galaxyBefore) || !mustModTime(t, provPath).Equal(provBefore) {
				t.Error("a second skip rewrote a sidecar the first one had already repaired")
			}
		})
	}
}

// TestScanInstalledTreeReadsProvenanceFromAnEarlierReleasesGalaxyYAML pins
// that outdated still reads a git or url kind from provenance keys left inside
// a legacy GALAXY.yml, rather than asking a Galaxy API about it.
func TestScanInstalledTreeReadsProvenanceFromAnEarlierReleasesGalaxyYAML(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		legacy   string
		col      collection
		wantKind installedKind
	}{
		{name: "git", col: schemaGitCollection(), legacy: "git_commit: " + strings.Repeat("b", 40), wantKind: installedGit},
		{name: "url", col: schemaURLCollection(), legacy: "url_sha256: " + strings.Repeat("a", 64), wantKind: installedURL},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, _ := seedInstalledCollection(t, tt.col, legacySidecar(tt.col, tt.legacy))
			cfg := &config.Config{DownloadPath: filepath.Dir(filepath.Dir(filepath.Dir(target.path)))}

			_, prov, ok := readInstalledSidecar(target.root, cfg, infra.New(noopPrinter{}, nil), path.Base(target.info))
			if !ok || installedKindOf(prov) != tt.wantKind {
				t.Errorf("kind = %v (ok=%v), want %v from the keys inside GALAXY.yml", installedKindOf(prov), ok, tt.wantKind)
			}
		})
	}
}

// TestReconcileGalaxyInfoKeepsTheOnlyProvenanceCopy pins that when the
// provenance file cannot be written, a legacy GALAXY.yml holding the only copy
// of the provenance is left untouched.
func TestReconcileGalaxyInfoKeepsTheOnlyProvenanceCopy(t *testing.T) {
	t.Parallel()
	col := schemaGitCollection()
	legacy := legacySidecar(col, "git_commit: "+strings.Repeat("b", 40))
	target, st := seedInstalledCollection(t, col, legacy)
	cfg := &config.Config{DownloadPath: filepath.Dir(filepath.Dir(filepath.Dir(target.path)))}
	// A non-empty directory at the provenance name: neither removable by the
	// repair's Remove nor writable as a file.
	blocker := filepath.Join(cfg.DownloadPath, filepath.FromSlash(target.info), provenanceFileName)
	if err := os.MkdirAll(filepath.Join(blocker, "occupied"), helpers.DirMod); err != nil {
		t.Fatalf("plant blocker: %v", err)
	}
	printer := &capturingPrinter{}

	state, ok := canSkipInstall(target, col, st, printer)
	if !ok {
		t.Fatal("canSkipInstall = false")
	}
	reconcileGalaxyInfo(infra.New(printer, nil), target, cfg, col, state)

	if data, _ := readInfoFile(t, cfg, target, galaxyYAMLFileName); string(data) != string(legacy) {
		t.Errorf("GALAXY.yml was rewritten though its provenance had nowhere to go:\n%s", data)
	}
	assertWarnedAbout(t, printer, provenanceFileName)
}

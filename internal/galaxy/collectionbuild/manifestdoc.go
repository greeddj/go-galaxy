package collectionbuild

import (
	"bytes"
	"encoding/json"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
)

const (
	// manifestFormat is the MANIFEST_FORMAT ansible writes on every row and
	// at the top level of both documents.
	manifestFormat = 1
	ftypeFile      = "file"
	ftypeDir       = "dir"
	chksumSHA256   = "sha256"
	rootRowName    = treearchive.RootName
)

// filesRow is one FILES.json row as ansible's _make_entry shapes it: a
// directory carries null for both checksum fields, which the pointers produce.
type filesRow struct {
	ChksumType   *string `json:"chksum_type"`
	ChksumSha256 *string `json:"chksum_sha256"`
	Name         string  `json:"name"`
	Ftype        string  `json:"ftype"`
	Format       int     `json:"format"`
}

type filesDoc struct {
	Files  []filesRow `json:"files"`
	Format int        `json:"format"`
}

// collectionInfo is the collection_info block of MANIFEST.json as
// ansible's _build_manifest writes it. license_file is null when empty.
type collectionInfo struct {
	Dependencies  map[string]string `json:"dependencies"`
	LicenseFile   *string           `json:"license_file"`
	Namespace     string            `json:"namespace"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Readme        string            `json:"readme"`
	Description   string            `json:"description"`
	Repository    string            `json:"repository"`
	Documentation string            `json:"documentation"`
	Homepage      string            `json:"homepage"`
	Issues        string            `json:"issues"`
	Authors       []string          `json:"authors"`
	License       []string          `json:"license"`
	Tags          []string          `json:"tags"`
}

// pointerRow is file_manifest_file: the FILES.json pointer that binds the
// listing to the manifest by digest.
type pointerRow struct {
	Name         string `json:"name"`
	Ftype        string `json:"ftype"`
	ChksumType   string `json:"chksum_type"`
	ChksumSha256 string `json:"chksum_sha256"`
	Format       int    `json:"format"`
}

type manifestDoc struct {
	CollectionInfo   collectionInfo `json:"collection_info"`
	FileManifestFile pointerRow     `json:"file_manifest_file"`
	Format           int            `json:"format"`
}

func dirRow(name string) filesRow {
	return filesRow{Name: name, Ftype: ftypeDir, Format: manifestFormat}
}

func fileRow(name, digest string) filesRow {
	chksumType := chksumSHA256
	return filesRow{Name: name, Ftype: ftypeFile, ChksumType: &chksumType, ChksumSha256: &digest, Format: manifestFormat}
}

// encodeDocuments renders FILES.json, then MANIFEST.json pointing at its exact
// bytes' digest. Both match Python's json.dumps(indent=True): a one-space
// indent and no HTML escaping, so a ">=1.0.0" constraint reads as written.
func encodeDocuments(meta *GalaxyYML, rows []filesRow) ([]byte, []byte, error) {
	filesJSON, err := encodeIndented(filesDoc{Files: rows, Format: manifestFormat})
	if err != nil {
		return nil, nil, err
	}
	info := collectionInfo{
		Dependencies:  nonNilMap(meta.Dependencies),
		Namespace:     meta.Namespace,
		Name:          meta.Name,
		Version:       meta.Version,
		Readme:        meta.Readme,
		Description:   meta.Description,
		Repository:    meta.Repository,
		Documentation: meta.Documentation,
		Homepage:      meta.Homepage,
		Issues:        meta.Issues,
		Authors:       nonNilList(meta.Authors),
		License:       nonNilList(meta.License),
		Tags:          nonNilList(meta.Tags),
	}
	if meta.LicenseFile != "" {
		licenseFile := meta.LicenseFile
		info.LicenseFile = &licenseFile
	}
	doc := manifestDoc{
		CollectionInfo: info,
		FileManifestFile: pointerRow{
			Name:         helpers.FilesManifestFileName,
			Ftype:        ftypeFile,
			ChksumType:   chksumSHA256,
			ChksumSha256: sha256Hex(filesJSON),
			Format:       manifestFormat,
		},
		Format: manifestFormat,
	}
	manifestJSON, err := encodeIndented(doc)
	if err != nil {
		return nil, nil, err
	}
	return manifestJSON, filesJSON, nil
}

// encodeIndented marshals v with a one-space indent and no HTML escaping,
// without the trailing newline json.Encoder adds.
func encodeIndented(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func nonNilList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

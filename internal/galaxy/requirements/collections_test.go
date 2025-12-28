package requirements

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestParseCollectionsStringList(t *testing.T) {
	t.Parallel()
	input := "- community.general\n- ansible.posix\n"
	collections, rolesFound, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if rolesFound {
		t.Fatalf("unexpected rolesFound")
	}
	if len(collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(collections))
	}
	if collections[0].Namespace != "community" || collections[0].Name != "general" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
	if collections[0].Version != "*" {
		t.Fatalf("expected default version '*', got %q", collections[0].Version)
	}
	if collections[0].Source != "https://default" {
		t.Fatalf("expected default source, got %q", collections[0].Source)
	}
}

func TestParseCollectionsRolesOnly(t *testing.T) {
	t.Parallel()
	input := "roles:\n  - geerlingguy.foo\n"
	collections, rolesFound, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if collections != nil {
		t.Fatalf("expected nil collections, got %#v", collections)
	}
}

func TestParseCollectionsUnsupportedFormat(t *testing.T) {
	t.Parallel()
	input := "foo: bar\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
		t.Fatalf("expected ErrUnsupportedRequirementsFormat, got %v", err)
	}
}

func TestParseCollectionsUnsupportedSource(t *testing.T) {
	t.Parallel()
	input := "- https://example.com/collections\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrUnsupportedCollectionSource) {
		t.Fatalf("expected ErrUnsupportedCollectionSource, got %v", err)
	}
}

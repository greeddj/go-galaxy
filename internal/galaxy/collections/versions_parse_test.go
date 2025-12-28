package collections

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestParseVersionsPayloadData(t *testing.T) {
	t.Parallel()
	payload := map[string]any{
		"data": []any{
			map[string]any{"version": "1.2.3"},
			map[string]any{"version": "2.0.0"},
		},
		"meta": map[string]any{"count": 2},
	}
	versions, total, err := parseVersionsPayload(payload)
	if err != nil {
		t.Fatalf("parseVersionsPayload error: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected total 2, got %d", total)
	}
	if len(versions) != 2 || versions[0] != "1.2.3" || versions[1] != "2.0.0" {
		t.Fatalf("unexpected versions: %#v", versions)
	}
}

func TestParseVersionsPayloadResults(t *testing.T) {
	t.Parallel()
	payload := map[string]any{
		"results": []any{
			map[string]any{"version": "0.1.0"},
		},
		"count": float64(5),
	}
	versions, total, err := parseVersionsPayload(payload)
	if err != nil {
		t.Fatalf("parseVersionsPayload error: %v", err)
	}
	if total != 5 {
		t.Fatalf("expected total 5, got %d", total)
	}
	if len(versions) != 1 || versions[0] != "0.1.0" {
		t.Fatalf("unexpected versions: %#v", versions)
	}
}

func TestParseVersionsPayloadEmpty(t *testing.T) {
	t.Parallel()
	_, _, err := parseVersionsPayload(nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrVersionsPayloadEmpty) {
		t.Fatalf("expected ErrVersionsPayloadEmpty, got %v", err)
	}
}

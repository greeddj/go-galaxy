package collections

import (
	"fmt"
	"sync"
	"testing"
)

// TestAPIRootMemoWinnerAbsentByDefault asserts that a fresh memo reports no
// winner for any base until one is recorded.
func TestAPIRootMemoWinnerAbsentByDefault(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()

	if apiRoot, ok := memo.winner("https://galaxy.example.com"); ok {
		t.Fatalf("expected no winner recorded, got (%q, true)", apiRoot)
	}
}

// TestAPIRootMemoRecordThenWinner asserts that a recorded winner is returned
// verbatim, and that an unrelated base is unaffected.
func TestAPIRootMemoRecordThenWinner(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()
	const base = "https://galaxy.example.com"
	const apiRoot = "https://galaxy.example.com/api/v2"

	memo.recordWinner(base, apiRoot)

	got, ok := memo.winner(base)
	if !ok {
		t.Fatal("expected a winner to be recorded, got none")
	}
	if got != apiRoot {
		t.Fatalf("expected winner %q, got %q", apiRoot, got)
	}

	if _, ok := memo.winner("https://other.example.com"); ok {
		t.Fatal("expected no winner recorded for an unrelated base")
	}
}

// TestAPIRootMemoRecordWinnerOverwrites asserts that recording a new winner
// for the same base replaces the previous one.
func TestAPIRootMemoRecordWinnerOverwrites(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()
	const base = "https://galaxy.example.com"

	memo.recordWinner(base, base+"/api/v2")
	memo.recordWinner(base, base+"/api/v3")

	got, ok := memo.winner(base)
	if !ok || got != base+"/api/v3" {
		t.Fatalf("expected the later recordWinner to win with %q, got (%q, %v)", base+"/api/v3", got, ok)
	}
}

// TestAPIRootMemoNilReceiverIsSafe asserts that a nil *apiRootMemo reports
// no winner and ignores recordWinner without panicking, since a collectionDeps
// literal may leave apiRoots unset.
func TestAPIRootMemoNilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var memo *apiRootMemo

	if apiRoot, ok := memo.winner("https://galaxy.example.com"); ok {
		t.Fatalf("expected no winner from a nil memo, got (%q, true)", apiRoot)
	}
	memo.recordWinner("https://galaxy.example.com", "https://galaxy.example.com/api/v3")
}

// TestAPIRootMemoConcurrentAccess drives recordWinner and winner from many
// goroutines across several bases, as the shared worker pools do; under
// -race it proves the RWMutex guards every access.
func TestAPIRootMemoConcurrentAccess(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()
	const bases = 8
	const goroutinesPerBase = 16

	var wg sync.WaitGroup
	for b := range bases {
		base := fmt.Sprintf("https://galaxy%d.example.com", b)
		apiRoot := base + "/api/v3"
		for range goroutinesPerBase {
			wg.Go(func() {
				memo.recordWinner(base, apiRoot)
				if got, ok := memo.winner(base); ok && got != apiRoot {
					t.Errorf("base %q: expected winner %q or none, got %q", base, apiRoot, got)
				}
			})
		}
	}
	wg.Wait()

	for b := range bases {
		base := fmt.Sprintf("https://galaxy%d.example.com", b)
		want := base + "/api/v3"
		got, ok := memo.winner(base)
		if !ok || got != want {
			t.Fatalf("base %q: expected winner %q, got (%q, %v)", base, want, got, ok)
		}
	}
}

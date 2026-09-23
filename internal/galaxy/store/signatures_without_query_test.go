package store

import "testing"

// TestSignaturesWithoutQueryNoHitReusesBackingArray pins that with no query to
// cut signaturesWithoutQuery returns its input slice itself, not a copy, while
// a cut returns a slice independent of the caller's backing array.
func TestSignaturesWithoutQueryNoHitReusesBackingArray(t *testing.T) {
	t.Parallel()
	sources := []string{
		"https://sigs.example.com/a.asc",
		"https://sigs.example.com/b.asc",
	}

	got := signaturesWithoutQuery(sources)

	// A mutation of the original after the call is visible through got only
	// when both view the same backing array.
	sources[0] = "https://sigs.example.com/mutated.asc"
	if got[0] != sources[0] {
		t.Fatalf("got[0] = %q after mutating sources[0] to %q, want the same backing array so the mutation is visible through got",
			got[0], sources[0])
	}

	// Positive control: a source with a query is cut into a fresh slice, so
	// the aliasing above is not the function always returning its argument.
	withQuery := []string{"https://sigs.example.com/c.asc?tok=SECRET"}
	cut := signaturesWithoutQuery(withQuery)
	if len(cut) != 1 || cut[0] != "https://sigs.example.com/c.asc" {
		t.Fatalf("signaturesWithoutQuery(%v) = %v, want the query cut to %q", withQuery, cut, "https://sigs.example.com/c.asc")
	}
	withQuery[0] = "https://sigs.example.com/c.asc?tok=CHANGED"
	if cut[0] == withQuery[0] {
		t.Fatalf("cut[0] changed after mutating the original query-bearing input, want an independent backing array on the cut path")
	}
}

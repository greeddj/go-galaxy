package s3

import (
	"bytes"
	"net/http"
	"sync"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// newSigningKeyTestClient builds a Client wired only for exercising
// signingKeyForDate - it never performs a real HTTP round trip, so a bare
// *http.Client and placeholder credentials are sufficient.
func newSigningKeyTestClient(t *testing.T) *Client {
	t.Helper()
	cfg := config.S3CacheConfig{
		Endpoint:  "https://s3.example.com",
		Bucket:    "test-bucket",
		Region:    "us-east-1",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: config.NewSecret("secret"),
	}
	c, err := newClient(cfg, &http.Client{})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

// TestSigningKeyMemoizedEqualsFreshDerivation pins that the memoized lookup
// returns the key a fresh deriveSigningKey call produces for the same date,
// region and secret.
func TestSigningKeyMemoizedEqualsFreshDerivation(t *testing.T) {
	t.Parallel()
	c := newSigningKeyTestClient(t)

	const date = "20260724"
	got := c.signingKeyForDate(date)
	want := deriveSigningKey(c.cfg.SecretKey.Reveal(), date, c.cfg.Region)
	if !bytes.Equal(got, want) {
		t.Fatalf("signingKeyForDate(%q) = %x, want %x", date, got, want)
	}
}

// TestSigningKeyRecomputesOnDateChange pins that a same-date call hits the
// cache, a new date recomputes, and a return to the earlier date recomputes
// rather than serving the intervening date's key.
func TestSigningKeyRecomputesOnDateChange(t *testing.T) {
	t.Parallel()
	c := newSigningKeyTestClient(t)

	const dateOne = "20260724"
	const dateTwo = "20260725"

	k1 := c.signingKeyForDate(dateOne)
	k1Again := c.signingKeyForDate(dateOne)
	if &k1[0] != &k1Again[0] {
		t.Fatal("expected the second same-date call to return the cached slice, not a re-derived one")
	}

	k2 := c.signingKeyForDate(dateTwo)
	if bytes.Equal(k2, k1) {
		t.Fatal("expected the signing key to change when the date rolls over")
	}
	wantK2 := deriveSigningKey(c.cfg.SecretKey.Reveal(), dateTwo, c.cfg.Region)
	if !bytes.Equal(k2, wantK2) {
		t.Fatalf("signingKeyForDate(%q) = %x, want %x", dateTwo, k2, wantK2)
	}

	// The cache now holds dateTwo, so a call back to dateOne must recompute
	// rather than returning the stale dateTwo key.
	k1Recomputed := c.signingKeyForDate(dateOne)
	wantK1 := deriveSigningKey(c.cfg.SecretKey.Reveal(), dateOne, c.cfg.Region)
	if !bytes.Equal(k1Recomputed, wantK1) {
		t.Fatalf("signingKeyForDate(%q) after rollback = %x, want %x", dateOne, k1Recomputed, wantK1)
	}
}

// TestSigningKeyConcurrent pins that signingKeyForDate stays correct under
// -race across two dates, since one Client is shared by the prefetch and
// install workers.
func TestSigningKeyConcurrent(t *testing.T) {
	t.Parallel()
	c := newSigningKeyTestClient(t)

	dates := []string{"20260724", "20260725"}
	wants := make(map[string][]byte, len(dates))
	for _, date := range dates {
		wants[date] = deriveSigningKey(c.cfg.SecretKey.Reveal(), date, c.cfg.Region)
	}

	const goroutines = 50
	const iterationsPerGoroutine = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			for j := range iterationsPerGoroutine {
				date := dates[(idx+j)%len(dates)]
				got := c.signingKeyForDate(date)
				if !bytes.Equal(got, wants[date]) {
					t.Errorf("signingKeyForDate(%q) = %x, want %x", date, got, wants[date])
				}
			}
		}(i)
	}
	wg.Wait()
}

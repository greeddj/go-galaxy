package collections_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// TestFrozenInstallCorruptedPinPropagatesBothSentinels pins that a frozen
// Start with a wrong sha256 pin fails matching both ErrSHA256Mismatch and
// ErrInstallationFailed at Start, and installs once the true pin is restored.
func TestFrozenInstallCorruptedPinPropagatesBothSentinels(t *testing.T) {
	t.Parallel()
	f := newE2EFixture(t)
	lockPath, lf := newFrozenPinFixture(t, f)

	setAppPin(lf, corruptedAppSHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("save corrupted lockfile: %v", err)
	}

	startErr := collections.Start(context.Background(), f.cfg, f.runtime)
	if startErr == nil {
		t.Fatal("expected an error from a corrupted lockfile pin, got nil")
	}
	if !errors.Is(startErr, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is(startErr, helpers.ErrSHA256Mismatch), got %v", startErr)
	}
	if !errors.Is(startErr, helpers.ErrInstallationFailed) {
		t.Fatalf("expected errors.Is(startErr, helpers.ErrInstallationFailed), got %v", startErr)
	}
	assertPathAbsent(t, installPathFor(f.downloadPath, "app"))

	// Positive control: the same fixture with the true pin installs, so the
	// failure above came from the pin and not from the fixture.
	setAppPin(lf, f.appV1.SHA256)
	if err := lockfile.Save(lockPath, lf); err != nil {
		t.Fatalf("restore the true pin in the lockfile: %v", err)
	}
	if err := os.RemoveAll(f.downloadPath); err != nil {
		t.Fatalf("remove downloadPath before the recovery run: %v", err)
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("recovery Start with the true pin restored: %v", err)
	}
	assertManifestInstalled(t, f.downloadPath, "app")
}

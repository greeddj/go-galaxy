package local

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// errArtifactNotFoundAfterCommit and errArtifactMetaNotFoundAfterCommit are
// static sentinels for readBackTestArtifact's two presence checks, declared
// at package level per err113 rather than as inline errors.New calls.
var (
	errArtifactNotFoundAfterCommit     = errors.New("Has reported the key absent right after Commit")
	errArtifactMetaNotFoundAfterCommit = errors.New("Meta reported the key absent right after Commit")
)

// TestArtifactsSafeForConcurrentUse pins ArtifactStore's goroutine safety on
// the local backend: eight goroutines run the full lifecycle under -race, each
// on its own key, since the contract promises safety only across distinct keys.
func TestArtifactsSafeForConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	artifacts := NewArtifacts(dir)

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Go(func() {
			errs <- exerciseArtifactKey(artifacts, fmt.Sprintf("k%02d", i))
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// exerciseArtifactKey commits key and reads it back through Has, Meta, Fetch
// and Delete, returning the first error; each caller uses its own key.
func exerciseArtifactKey(artifacts *Artifacts, key string) error {
	if err := commitTestArtifact(artifacts, key); err != nil {
		return err
	}
	return readBackTestArtifact(artifacts, key)
}

// commitTestArtifact creates a temp file, writes a few bytes naming key, and
// commits it under key with a validly-shaped sha256 in its metadata.
func commitTestArtifact(artifacts *Artifacts, key string) error {
	ctx := context.Background()

	file, cleanupTemp, err := artifacts.TempFile(ctx, ".artifact-")
	if err != nil {
		return fmt.Errorf("key %s: TempFile: %w", key, err)
	}
	defer cleanupTemp()

	if _, err := file.WriteString("artifact bytes for " + key); err != nil {
		return fmt.Errorf("key %s: write temp file: %w", key, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("key %s: close temp file: %w", key, err)
	}

	if _, err := artifacts.Commit(ctx, key, file.Name(), map[string]string{"sha256": testSHA}); err != nil {
		return fmt.Errorf("key %s: Commit: %w", key, err)
	}
	return nil
}

// readBackTestArtifact runs Has, Meta, Fetch, and Delete against key, all of
// which a prior commitTestArtifact(artifacts, key) must satisfy.
func readBackTestArtifact(artifacts *Artifacts, key string) error {
	ctx := context.Background()

	found, err := artifacts.Has(ctx, key)
	if err != nil {
		return fmt.Errorf("key %s: Has: %w", key, err)
	}
	if !found {
		return fmt.Errorf("key %s: %w", key, errArtifactNotFoundAfterCommit)
	}

	_, found, err = artifacts.Meta(ctx, key)
	if err != nil {
		return fmt.Errorf("key %s: Meta: %w", key, err)
	}
	if !found {
		return fmt.Errorf("key %s: %w", key, errArtifactMetaNotFoundAfterCommit)
	}

	fetched, err := artifacts.Fetch(ctx, key)
	if err != nil {
		return fmt.Errorf("key %s: Fetch: %w", key, err)
	}
	if fetched.Cleanup != nil {
		fetched.Cleanup()
	}

	if err := artifacts.Delete(ctx, key); err != nil {
		return fmt.Errorf("key %s: Delete: %w", key, err)
	}
	return nil
}

package cache

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/cache/s3"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// testS3Config is an enabled S3 cache configuration; New never dials it.
func testS3Config() config.S3CacheConfig {
	return config.S3CacheConfig{
		Endpoint:  "http://127.0.0.1:1",
		Bucket:    "cache",
		Region:    "us-east-1",
		AccessKey: "x",
		SecretKey: config.NewSecret("y"),
		PathStyle: true,
		Enabled:   true,
	}
}

// TestNewRefusesANilConfig pins that a missing config is the shared nil-config
// sentinel, so it exits like every other nil-config refusal.
func TestNewRefusesANilConfig(t *testing.T) {
	t.Parallel()
	backend, err := New(nil, &infra.Infra{HTTP: http.DefaultClient})
	if !errors.Is(err, helpers.ErrConfigIsNil) {
		t.Fatalf("New(nil) error = %v, want helpers.ErrConfigIsNil", err)
	}
	if backend != nil {
		t.Fatalf("New(nil) backend = %v, want nil", backend)
	}
}

// TestNewS3NeedsAnHTTPClient pins that the S3 backend is refused without a
// runtime or without its HTTP client, before any backend is built.
func TestNewS3NeedsAnHTTPClient(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{S3Cache: testS3Config()}
	for name, runtime := range map[string]*infra.Infra{"nil runtime": nil, "nil HTTP client": {}} {
		backend, err := New(cfg, runtime)
		if !errors.Is(err, errHTTPClientNil) {
			t.Errorf("%s: New error = %v, want errHTTPClientNil", name, err)
		}
		if backend != nil {
			t.Errorf("%s: New backend = %v, want nil", name, backend)
		}
	}
}

// TestNewSelectsS3WhenEnabled pins that an enabled S3 cache yields the S3
// backend, consulting the runtime's TempDir exactly once.
func TestNewSelectsS3WhenEnabled(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{S3Cache: testS3Config(), CacheDir: filepath.Join(t.TempDir(), "unused")}
	tempDirCalls := 0
	runtime := &infra.Infra{HTTP: http.DefaultClient, TempDir: func() string {
		tempDirCalls++
		return t.TempDir()
	}}

	backend, err := New(cfg, runtime)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	if _, ok := backend.(*s3.Backend); !ok {
		t.Fatalf("New backend = %T, want *s3.Backend", backend)
	}
	if tempDirCalls != 1 {
		t.Fatalf("runtime.TempDir called %d times, want 1", tempDirCalls)
	}
	if _, err := os.Stat(cfg.CacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(CacheDir) = %v, want the local cache dir left untouched", err)
	}
}

// TestNewS3WithoutTempDirStillBuilds pins that a runtime with no TempDir
// function is accepted, the backend falling back to its own default.
func TestNewS3WithoutTempDirStillBuilds(t *testing.T) {
	t.Parallel()
	backend, err := New(&config.Config{S3Cache: testS3Config()}, &infra.Infra{HTTP: http.DefaultClient})
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	if _, ok := backend.(*s3.Backend); !ok {
		t.Fatalf("New backend = %T, want *s3.Backend", backend)
	}
}

// TestNewReturnsTheS3ConstructorError pins that an S3 configuration the
// backend refuses is reported, not replaced by a local fallback.
func TestNewReturnsTheS3ConstructorError(t *testing.T) {
	t.Parallel()
	s3cfg := testS3Config()
	s3cfg.Bucket = ""
	backend, err := New(&config.Config{S3Cache: s3cfg, CacheDir: t.TempDir()}, &infra.Infra{HTTP: http.DefaultClient})
	if err == nil {
		t.Fatalf("New with no bucket = %T, want an error", backend)
	}
	if _, isLocal := backend.(*local.Backend); isLocal {
		t.Fatal("New with no bucket fell back to the local backend")
	}
}

// TestNewSelectsLocalOtherwise pins that a disabled S3 cache yields the local
// backend rooted at CacheDir, which needs no runtime at all.
func TestNewSelectsLocalOtherwise(t *testing.T) {
	t.Parallel()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	backend, err := New(&config.Config{CacheDir: cacheDir}, nil)
	if err != nil {
		t.Fatalf("New error = %v", err)
	}
	if _, ok := backend.(*local.Backend); !ok {
		t.Fatalf("New backend = %T, want *local.Backend", backend)
	}
	if err := backend.Open(context.Background()); err != nil {
		t.Fatalf("Open error = %v", err)
	}
	info, err := os.Stat(cacheDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("Stat(%s) = %v, %v; want the local backend to create CacheDir", cacheDir, info, err)
	}
}
